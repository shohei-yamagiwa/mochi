package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v93/github"
)

// ClientFactory provides installation-scoped GitHub REST API clients and ephemeral read-only clone tokens.
type ClientFactory interface {
	ClientForInstallation(ctx context.Context, installationID int64) (*github.Client, error)
	CloneTokenForRepo(ctx context.Context, installationID int64, repo string) (string, error)
}

// AppConfig configures GitHub App or static-token authentication.
type AppConfig struct {
	AppID          int64
	PrivateKey     []byte
	PrivateKeyPath string
	StaticToken    string
	BaseURL        string
	UploadURL      string
	HTTPClient     *http.Client
}

type appClientFactory struct {
	appID         int64
	appsTr        *ghinstallation.AppsTransport
	baseURL       string
	uploadURL     string
	baseTransport http.RoundTripper
	httpClient    *http.Client
	staticToken   string
}

type staticClientFactory struct {
	client     *github.Client
	cloneToken string
}

// NewClientFactory builds a ClientFactory using GitHub App credentials when AppID is set,
// or falling back to StaticToken (PAT / GITHUB_TOKEN).
func NewClientFactory(cfg AppConfig) (ClientFactory, error) {
	baseTr := http.DefaultTransport
	if cfg.HTTPClient != nil && cfg.HTTPClient.Transport != nil {
		baseTr = cfg.HTTPClient.Transport
	}

	if cfg.AppID > 0 {
		keyBytes := cfg.PrivateKey
		if len(keyBytes) == 0 && strings.TrimSpace(cfg.PrivateKeyPath) != "" {
			loaded, err := os.ReadFile(strings.TrimSpace(cfg.PrivateKeyPath))
			if err != nil {
				return nil, fmt.Errorf("read github app private key %q: %w", cfg.PrivateKeyPath, err)
			}
			keyBytes = loaded
		}
		if len(keyBytes) == 0 {
			return nil, fmt.Errorf("github app private key is required when app_id is configured")
		}

		appsTr, err := ghinstallation.NewAppsTransport(baseTr, cfg.AppID, keyBytes)
		if err != nil {
			return nil, fmt.Errorf("init github app transport: %w", err)
		}
		if strings.TrimSpace(cfg.BaseURL) != "" {
			appsTr.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		}
		return &appClientFactory{
			appID:         cfg.AppID,
			appsTr:        appsTr,
			baseURL:       strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
			uploadURL:     strings.TrimRight(strings.TrimSpace(cfg.UploadURL), "/"),
			baseTransport: baseTr,
			httpClient:    cfg.HTTPClient,
			staticToken:   cfg.StaticToken,
		}, nil
	}

	if strings.TrimSpace(cfg.StaticToken) != "" || strings.TrimSpace(cfg.BaseURL) != "" {
		var opts []github.ClientOptionsFunc
		if cfg.HTTPClient != nil {
			opts = append(opts, github.WithHTTPClient(cfg.HTTPClient))
		}
		if tok := strings.TrimSpace(cfg.StaticToken); tok != "" {
			opts = append(opts, github.WithAuthToken(tok))
		}
		if base := strings.TrimSpace(cfg.BaseURL); base != "" {
			upload := strings.TrimSpace(cfg.UploadURL)
			if upload == "" {
				upload = base
			}
			opts = append(opts, github.WithEnterpriseURLs(base, upload))
		}
		client, err := github.NewClient(opts...)
		if err != nil {
			return nil, fmt.Errorf("create static github client: %w", err)
		}
		return &staticClientFactory{
			client:     client,
			cloneToken: strings.TrimSpace(cfg.StaticToken),
		}, nil
	}

	return nil, fmt.Errorf("github authentication requires either app_id+private_key or static_token")
}

// NewStaticClientFactory creates a ClientFactory backed by a pre-configured *github.Client and clone token.
func NewStaticClientFactory(client *github.Client, cloneToken string) ClientFactory {
	return &staticClientFactory{
		client:     client,
		cloneToken: cloneToken,
	}
}

func (f *appClientFactory) ClientForInstallation(_ context.Context, installationID int64) (*github.Client, error) {
	if installationID <= 0 {
		if f.staticToken != "" {
			return f.newClientWithHTTP(&http.Client{Transport: f.baseTransport}, f.staticToken)
		}
		return nil, fmt.Errorf("github installation_id is required for GitHub App authentication")
	}

	itr := ghinstallation.NewFromAppsTransport(f.appsTr, installationID)
	if f.baseURL != "" {
		itr.BaseURL = f.baseURL
	}
	hc := &http.Client{Transport: itr}
	if f.httpClient != nil {
		hc.Timeout = f.httpClient.Timeout
	}
	return f.newClientWithHTTP(hc, "")
}

func (f *appClientFactory) CloneTokenForRepo(ctx context.Context, installationID int64, repo string) (string, error) {
	if installationID <= 0 {
		return f.staticToken, nil
	}
	hc := &http.Client{Transport: f.appsTr}
	if f.httpClient != nil {
		hc.Timeout = f.httpClient.Timeout
	}
	appClient, err := f.newClientWithHTTP(hc, "")
	if err != nil {
		return "", err
	}

	opts := &github.InstallationTokenOptions{
		Permissions: &github.InstallationPermissions{
			Contents: github.Ptr("read"),
		},
	}
	if strings.TrimSpace(repo) != "" {
		opts.Repositories = []string{strings.TrimSpace(repo)}
	}
	tok, _, err := appClient.Apps.CreateInstallationToken(ctx, installationID, opts)
	if err != nil {
		return "", fmt.Errorf("create read-only installation clone token for %q: %w", repo, err)
	}
	return tok.GetToken(), nil
}

func (f *appClientFactory) newClientWithHTTP(hc *http.Client, authToken string) (*github.Client, error) {
	var opts []github.ClientOptionsFunc
	if hc != nil {
		opts = append(opts, github.WithHTTPClient(hc))
	}
	if authToken != "" {
		opts = append(opts, github.WithAuthToken(authToken))
	}
	if f.baseURL != "" {
		upload := f.uploadURL
		if upload == "" {
			upload = f.baseURL
		}
		opts = append(opts, github.WithEnterpriseURLs(f.baseURL, upload))
	}
	c, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create github client: %w", err)
	}
	return c, nil
}

func (f *staticClientFactory) ClientForInstallation(_ context.Context, _ int64) (*github.Client, error) {
	if f.client == nil {
		return nil, fmt.Errorf("static github client is nil")
	}
	return f.client, nil
}

func (f *staticClientFactory) CloneTokenForRepo(_ context.Context, _ int64, _ string) (string, error) {
	return f.cloneToken, nil
}
