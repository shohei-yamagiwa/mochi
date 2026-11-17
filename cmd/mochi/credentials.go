package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/credentials"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
	"github.com/spf13/cobra"
)

type credentialsTargetOpts struct {
	serverURL string
	apiToken  string
	dbPath    string
	masterKey string
}

func newCredentialsCmd() *cobra.Command {
	opts := &credentialsTargetOpts{}

	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Manage per-repository encrypted LLM API keys",
		Long: `Manage per-repository encrypted LLM API keys.

By default, commands operate directly on the local SQLite database (--db / MOCHI_DB_PATH)
using --master-key / MOCHI_MASTER_KEY. When --server or MOCHI_SERVER_URL is set, commands
call the Mochi HTTP API using --api-token / MOCHI_API_TOKEN instead.`,
	}

	cmd.PersistentFlags().StringVar(&opts.serverURL, "server", os.Getenv("MOCHI_SERVER_URL"), "Mochi HTTP server URL (e.g. https://mochi.example.com); uses local SQLite when empty")
	cmd.PersistentFlags().StringVar(&opts.apiToken, "api-token", os.Getenv("MOCHI_API_TOKEN"), "Bearer token for remote Mochi server API")
	cmd.PersistentFlags().StringVar(&opts.dbPath, "db", envDefault("MOCHI_DB_PATH", "mochi.db"), "SQLite database path for local mode")
	cmd.PersistentFlags().StringVar(&opts.masterKey, "master-key", os.Getenv("MOCHI_MASTER_KEY"), "32-byte AES-256 master key (64 hex chars or base64) for local SQLite encryption")

	cmd.AddCommand(
		newCredentialsSetCmd(opts),
		newCredentialsListCmd(opts),
		newCredentialsDeleteCmd(opts),
	)
	return cmd
}

func newCredentialsSetCmd(opts *credentialsTargetOpts) *cobra.Command {
	var (
		apiKey      string
		apiKeyStdin bool
		baseURL     string
	)

	cmd := &cobra.Command{
		Use:   "set <owner/repo> <provider>",
		Short: "Encrypt and store an LLM provider API key for a repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseOwnerRepoArg(args[0])
			if err != nil {
				return err
			}
			provider, err := credentials.ValidateProvider(args[1])
			if err != nil {
				return err
			}

			if apiKeyStdin && strings.TrimSpace(apiKey) != "" {
				return fmt.Errorf("specify either --api-key or --api-key-stdin, not both")
			}
			keyVal := apiKey
			if apiKeyStdin {
				raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
				if err != nil {
					return fmt.Errorf("read api key from stdin: %w", err)
				}
				keyVal = strings.TrimRight(string(raw), "\r\n")
			}
			if strings.TrimSpace(keyVal) == "" {
				return fmt.Errorf("API key is required (pass --api-key or --api-key-stdin)")
			}

			ctx := cmd.Context()
			if strings.TrimSpace(opts.serverURL) != "" {
				if err := setRemoteCredential(ctx, opts, owner, repo, provider, keyVal, baseURL); err != nil {
					return err
				}
			} else {
				if err := setLocalCredential(ctx, opts, owner, repo, provider, keyVal, baseURL); err != nil {
					return err
				}
			}

			if strings.TrimSpace(baseURL) != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved %s credential for %s/%s (base_url=%s)\n", provider, owner, repo, strings.TrimSpace(baseURL))
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved %s credential for %s/%s\n", provider, owner, repo)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&apiKey, "api-key", "", "Provider API key (prefer --api-key-stdin to avoid shell history)")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "Read provider API key from stdin")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Optional provider base URL override (required for openai_compat if not set in workflow)")
	return cmd
}

func newCredentialsListCmd(opts *credentialsTargetOpts) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list <owner/repo>",
		Short: "List configured LLM provider credentials for a repository (keys are never returned)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseOwnerRepoArg(args[0])
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			var creds []*domain.RepoCredentialSummary
			if strings.TrimSpace(opts.serverURL) != "" {
				creds, err = listRemoteCredentials(ctx, opts, owner, repo)
			} else {
				creds, err = listLocalCredentials(ctx, opts, owner, repo)
			}
			if err != nil {
				return err
			}

			if jsonOut {
				if creds == nil {
					creds = []*domain.RepoCredentialSummary{}
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"credentials": creds})
			}

			if len(creds) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No credentials configured for %s/%s.\n", owner, repo)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "PROVIDER\tBASE URL\tUPDATED")
			for _, c := range creds {
				base := c.BaseURL
				if base == "" {
					base = "-"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Provider, base, c.UpdatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print credential summaries as JSON")
	return cmd
}

func newCredentialsDeleteCmd(opts *credentialsTargetOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <owner/repo> <provider>",
		Short: "Delete a stored LLM provider credential for a repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, repo, err := parseOwnerRepoArg(args[0])
			if err != nil {
				return err
			}
			provider, err := credentials.ValidateProvider(args[1])
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			if strings.TrimSpace(opts.serverURL) != "" {
				if err := deleteRemoteCredential(ctx, opts, owner, repo, provider); err != nil {
					return err
				}
			} else {
				if err := deleteLocalCredential(ctx, opts, owner, repo, provider); err != nil {
					return err
				}
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %s credential for %s/%s\n", provider, owner, repo)
			return nil
		},
	}
	return cmd
}

func parseOwnerRepoArg(raw string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("repository must be in owner/repo format, got %q", raw)
	}
	return credentials.NormalizeRepo(parts[0], parts[1])
}

func openCredentialServiceLocal(ctx context.Context, opts *credentialsTargetOpts) (*sqlite.SQLiteStore, *credentials.Service, error) {
	cipher, err := credentials.NewCipherFromString(opts.masterKey)
	if err != nil {
		return nil, nil, err
	}
	st, err := sqlite.Open(ctx, opts.dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite store %q: %w", opts.dbPath, err)
	}
	svc, err := credentials.NewService(st, cipher)
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return st, svc, nil
}

func setLocalCredential(ctx context.Context, opts *credentialsTargetOpts, owner, repo, provider, apiKey, baseURL string) error {
	st, svc, err := openCredentialServiceLocal(ctx, opts)
	if err != nil {
		return err
	}
	defer st.Close()
	return svc.Set(ctx, owner, repo, provider, apiKey, baseURL)
}

func listLocalCredentials(ctx context.Context, opts *credentialsTargetOpts, owner, repo string) ([]*domain.RepoCredentialSummary, error) {
	st, err := sqlite.Open(ctx, opts.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store %q: %w", opts.dbPath, err)
	}
	defer st.Close()
	return st.ListRepoCredentials(ctx, owner, repo)
}

func deleteLocalCredential(ctx context.Context, opts *credentialsTargetOpts, owner, repo, provider string) error {
	st, err := sqlite.Open(ctx, opts.dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite store %q: %w", opts.dbPath, err)
	}
	defer st.Close()
	if err := st.DeleteRepoCredential(ctx, owner, repo, provider); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("credential for %s/%s (%s) not found", owner, repo, provider)
		}
		return err
	}
	return nil
}

func normalizeServerURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid --server URL %q: must be http:// or https://", raw)
	}
	return trimmed, nil
}

func doRemoteCredentialRequest(ctx context.Context, opts *credentialsTargetOpts, method, path string, body any) ([]byte, error) {
	base, err := normalizeServerURL(opts.serverURL)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := strings.TrimSpace(opts.apiToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBytes, &errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, errBody.Error)
		}
		return nil, fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	return respBytes, nil
}

func setRemoteCredential(ctx context.Context, opts *credentialsTargetOpts, owner, repo, provider, apiKey, baseURL string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/credentials/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(provider))
	payload := map[string]string{"api_key": apiKey}
	if trimmed := strings.TrimSpace(baseURL); trimmed != "" {
		payload["base_url"] = trimmed
	}
	_, err := doRemoteCredentialRequest(ctx, opts, http.MethodPut, path, payload)
	return err
}

func listRemoteCredentials(ctx context.Context, opts *credentialsTargetOpts, owner, repo string) ([]*domain.RepoCredentialSummary, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/credentials", url.PathEscape(owner), url.PathEscape(repo))
	raw, err := doRemoteCredentialRequest(ctx, opts, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Credentials []*domain.RepoCredentialSummary `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode credentials response: %w", err)
	}
	return payload.Credentials, nil
}

func deleteRemoteCredential(ctx context.Context, opts *credentialsTargetOpts, owner, repo, provider string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/credentials/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(provider))
	_, err := doRemoteCredentialRequest(ctx, opts, http.MethodDelete, path, nil)
	return err
}
