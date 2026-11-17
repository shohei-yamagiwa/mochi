package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/llm/anthropic"
	"github.com/shohei-yamagiwa/mochi/internal/llm/gemini"
	"github.com/shohei-yamagiwa/mochi/internal/llm/openai"
	"github.com/shohei-yamagiwa/mochi/internal/store"
)

const keyByteLen = 32

var validProviders = []string{"anthropic", "openai", "gemini", "openai_compat"}

// ParseMasterKey decodes a 32-byte AES-256 master key from either 64 hex characters or base64.
func ParseMasterKey(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("master key is required (set MOCHI_MASTER_KEY to a 32-byte hex or base64 key)")
	}
	if len(trimmed) == hex.EncodedLen(keyByteLen) {
		decoded, err := hex.DecodeString(trimmed)
		if err == nil && len(decoded) == keyByteLen {
			return decoded, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(trimmed)
		if err == nil && len(decoded) == keyByteLen {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("master key must decode to %d bytes (64 hex chars or base64)", keyByteLen)
}

// ValidateProvider normalizes and validates a supported LLM provider name.
func ValidateProvider(provider string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(provider))
	if !slices.Contains(validProviders, norm) {
		return "", fmt.Errorf("unsupported provider %q (must be one of: %s)", provider, strings.Join(validProviders, ", "))
	}
	return norm, nil
}

// NormalizeRepo normalizes and validates a GitHub owner and repository name.
func NormalizeRepo(owner, repo string) (string, string, error) {
	normOwner := strings.ToLower(strings.TrimSpace(owner))
	normRepo := strings.ToLower(strings.TrimSpace(repo))
	if normOwner == "" || normRepo == "" {
		return "", "", fmt.Errorf("repository owner and name are required")
	}
	for _, part := range []string{normOwner, normRepo} {
		if strings.ContainsAny(part, "/\\ \t\r\n") || part == "." || part == ".." {
			return "", "", fmt.Errorf("invalid repository identifier %q/%q", owner, repo)
		}
	}
	return normOwner, normRepo, nil
}

// Cipher encrypts and decrypts per-repository API keys using AES-256-GCM bound to (owner, repo, provider).
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher constructs an AES-256-GCM Cipher from a 32-byte key.
func NewCipher(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != keyByteLen {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", keyByteLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("init aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// NewCipherFromString parses raw (hex or base64) and constructs a Cipher.
func NewCipherFromString(raw string) (*Cipher, error) {
	key, err := ParseMasterKey(raw)
	if err != nil {
		return nil, err
	}
	return NewCipher(key)
}

func credentialAAD(owner, repo, provider string) []byte {
	return []byte(strings.ToLower(strings.TrimSpace(owner)) + "\x00" +
		strings.ToLower(strings.TrimSpace(repo)) + "\x00" +
		strings.ToLower(strings.TrimSpace(provider)))
}

// Encrypt encrypts apiKey for (owner, repo, provider), returning ciphertext and nonce.
func (c *Cipher) Encrypt(owner, repo, provider, apiKey string) (ciphertext, nonce []byte, err error) {
	if c == nil || c.aead == nil {
		return nil, nil, fmt.Errorf("credential cipher is not initialized")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, nil, fmt.Errorf("api_key must not be empty")
	}
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return nil, nil, err
	}
	normProvider, err := ValidateProvider(provider)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	aad := credentialAAD(normOwner, normRepo, normProvider)
	ciphertext = c.aead.Seal(nil, nonce, []byte(apiKey), aad)
	return ciphertext, nonce, nil
}

// Decrypt decrypts a persisted RepoCredentialRecord.
func (c *Cipher) Decrypt(rec *domain.RepoCredentialRecord) (string, error) {
	if c == nil || c.aead == nil {
		return "", fmt.Errorf("credential cipher is not initialized")
	}
	if rec == nil {
		return "", fmt.Errorf("credential record must not be nil")
	}
	if len(rec.EncryptedAPIKey) == 0 || len(rec.Nonce) == 0 {
		return "", fmt.Errorf("stored credential for %s/%s (%s) is empty", rec.RepoOwner, rec.RepoName, rec.Provider)
	}
	if len(rec.Nonce) != c.aead.NonceSize() {
		return "", fmt.Errorf("invalid nonce length for %s/%s (%s)", rec.RepoOwner, rec.RepoName, rec.Provider)
	}
	aad := credentialAAD(rec.RepoOwner, rec.RepoName, rec.Provider)
	plain, err := c.aead.Open(nil, rec.Nonce, rec.EncryptedAPIKey, aad)
	if err != nil {
		return "", fmt.Errorf("decrypt credential for %s/%s (%s): %w", rec.RepoOwner, rec.RepoName, rec.Provider, err)
	}
	return string(plain), nil
}

// Service manages encrypted per-repository LLM provider credentials and strict per-repo Registry construction.
type Service struct {
	store  store.Store
	cipher *Cipher
}

// NewService constructs a Service backed by store and cipher.
func NewService(st store.Store, cipher *Cipher) (*Service, error) {
	if st == nil {
		return nil, fmt.Errorf("credentials service: store must not be nil")
	}
	if cipher == nil {
		return nil, fmt.Errorf("credentials service: cipher must not be nil")
	}
	return &Service{store: st, cipher: cipher}, nil
}

// Set validates, encrypts, and stores an API key (and optional baseURL) for a repository and provider.
func (s *Service) Set(ctx context.Context, owner, repo, provider, apiKey, baseURL string) error {
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return err
	}
	normProvider, err := ValidateProvider(provider)
	if err != nil {
		return err
	}
	trimmedURL := strings.TrimSpace(baseURL)
	if trimmedURL != "" {
		parsed, err := url.Parse(trimmedURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("invalid base_url %q: must be an http or https URL", baseURL)
		}
	}
	ciphertext, nonce, err := s.cipher.Encrypt(normOwner, normRepo, normProvider, apiKey)
	if err != nil {
		return err
	}
	return s.store.UpsertRepoCredential(ctx, &domain.RepoCredentialRecord{
		RepoOwner:       normOwner,
		RepoName:        normRepo,
		Provider:        normProvider,
		EncryptedAPIKey: ciphertext,
		Nonce:           nonce,
		BaseURL:         trimmedURL,
	})
}

// List returns non-secret credential metadata for a repository.
func (s *Service) List(ctx context.Context, owner, repo string) ([]*domain.RepoCredentialSummary, error) {
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return nil, err
	}
	return s.store.ListRepoCredentials(ctx, normOwner, normRepo)
}

// Delete removes a stored credential for a repository and provider.
func (s *Service) Delete(ctx context.Context, owner, repo, provider string) error {
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return err
	}
	normProvider, err := ValidateProvider(provider)
	if err != nil {
		return err
	}
	return s.store.DeleteRepoCredential(ctx, normOwner, normRepo, normProvider)
}

// ResolveRegistry builds an llm.Registry for the providers required by wf using only
// per-repository credentials stored for (owner, repo). It never falls back to server env vars.
func (s *Service) ResolveRegistry(ctx context.Context, owner, repo string, wf *domain.Workflow) (*llm.Registry, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflow must not be nil")
	}
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return nil, err
	}

	required := make([]string, 0, len(wf.Agents)+1)
	seen := make(map[string]struct{}, len(wf.Agents)+1)
	hasCompatRoleBaseURL := false

	addProvider := func(raw string, baseURL string) error {
		prov, err := ValidateProvider(raw)
		if err != nil {
			return err
		}
		if prov == "openai_compat" && strings.TrimSpace(baseURL) != "" {
			hasCompatRoleBaseURL = true
		}
		if _, ok := seen[prov]; !ok {
			seen[prov] = struct{}{}
			required = append(required, prov)
		}
		return nil
	}

	for _, a := range wf.Agents {
		if err := addProvider(a.LLM.Provider, a.LLM.BaseURL); err != nil {
			return nil, fmt.Errorf("agent %q: %w", a.Role, err)
		}
	}
	if err := addProvider(wf.Synthesizer.LLM.Provider, wf.Synthesizer.LLM.BaseURL); err != nil {
		return nil, fmt.Errorf("synthesizer: %w", err)
	}

	reg := llm.NewRegistry()
	for _, prov := range required {
		rec, err := s.store.GetRepoCredential(ctx, normOwner, normRepo, prov)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("missing %s API key for repository %s/%s: configure via 'mochi credentials set' or PUT /api/v1/repos/%s/%s/credentials/%s",
					prov, normOwner, normRepo, normOwner, normRepo, prov)
			}
			return nil, fmt.Errorf("load %s credential for %s/%s: %w", prov, normOwner, normRepo, err)
		}
		apiKey, err := s.cipher.Decrypt(rec)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("empty %s API key stored for repository %s/%s", prov, normOwner, normRepo)
		}

		p, err := buildProvider(prov, apiKey, rec.BaseURL, hasCompatRoleBaseURL)
		if err != nil {
			return nil, fmt.Errorf("init %s provider for %s/%s: %w", prov, normOwner, normRepo, err)
		}
		if err := reg.Register(p); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func buildProvider(provider, apiKey, baseURL string, hasCompatRoleBaseURL bool) (llm.Provider, error) {
	switch provider {
	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(baseURL))
		}
		return anthropic.NewProvider(opts...)
	case "openai":
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, openai.WithBaseURL(baseURL))
		}
		return openai.NewProvider(opts...)
	case "gemini":
		opts := []gemini.Option{gemini.WithAPIKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, gemini.WithBaseURL(baseURL))
		}
		return gemini.NewProvider(opts...)
	case "openai_compat":
		if strings.TrimSpace(baseURL) == "" && !hasCompatRoleBaseURL {
			return nil, fmt.Errorf("openai_compat requires base_url on the repository credential or workflow llm config")
		}
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, openai.WithBaseURL(baseURL))
		}
		return openai.NewCompatProvider(opts...)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

// RunnerResolver adapts Service.ResolveRegistry to orchestrator.Runner.LLMRegistryResolver.
func (s *Service) RunnerResolver() func(ctx context.Context, task *domain.TaskContext, wf *domain.Workflow) (*llm.Registry, error) {
	return func(ctx context.Context, task *domain.TaskContext, wf *domain.Workflow) (*llm.Registry, error) {
		if task == nil {
			return nil, fmt.Errorf("cannot resolve per-repository LLM credentials: task is nil")
		}
		var owner, repo string
		if task.Changeset != nil {
			owner = strings.TrimSpace(task.Changeset.RepoOwner)
			repo = strings.TrimSpace(task.Changeset.RepoName)
		}
		if owner == "" || repo == "" {
			if parsedOwner, parsedRepo, ok := parseRepoFromURL(task.Workspace.RepoURL); ok {
				owner, repo = parsedOwner, parsedRepo
			}
		}
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("cannot resolve per-repository LLM credentials: repository owner and name are missing")
		}
		return s.ResolveRegistry(ctx, owner, repo, wf)
	}
}

func parseRepoFromURL(raw string) (string, string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", false
	}
	var pathPart string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", false
		}
		pathPart = u.Path
	} else if idx := strings.Index(s, ":"); idx > 0 {
		// SCP-style git remote (e.g. git@github.com:owner/repo.git).
		pathPart = s[idx+1:]
	} else {
		pathPart = s
	}
	pathPart = strings.Trim(strings.TrimSuffix(pathPart, ".git"), "/")
	parts := strings.Split(pathPart, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	normOwner, normRepo, err := NormalizeRepo(owner, repo)
	if err != nil {
		return "", "", false
	}
	return normOwner, normRepo, true
}
