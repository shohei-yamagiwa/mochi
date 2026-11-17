package credentials_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/credentials"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
)

func testMasterKeyHex() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return hex.EncodeToString(key)
}

func TestParseMasterKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(0xA0 + i)
	}

	fromHex, err := credentials.ParseMasterKey(hex.EncodeToString(raw))
	if err != nil {
		t.Fatalf("ParseMasterKey(hex): %v", err)
	}
	if string(fromHex) != string(raw) {
		t.Fatal("hex key mismatch")
	}

	fromB64, err := credentials.ParseMasterKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("ParseMasterKey(base64): %v", err)
	}
	if string(fromB64) != string(raw) {
		t.Fatal("base64 key mismatch")
	}

	for _, bad := range []string{"", "short", "zzzz"} {
		if _, err := credentials.ParseMasterKey(bad); err == nil {
			t.Fatalf("ParseMasterKey(%q) should fail", bad)
		}
	}
}

func TestCipherEncryptDecryptAndAADBindsRepoAndProvider(t *testing.T) {
	c, err := credentials.NewCipherFromString(testMasterKeyHex())
	if err != nil {
		t.Fatalf("NewCipherFromString: %v", err)
	}

	ct, nonce, err := c.Encrypt("Shohei-Yamagiwa", "Mochi", "gemini", "gem-secret-123")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(string(ct), "gem-secret-123") {
		t.Fatal("ciphertext must not contain plaintext")
	}

	plain, err := c.Decrypt(&domain.RepoCredentialRecord{
		RepoOwner:       "shohei-yamagiwa",
		RepoName:        "mochi",
		Provider:        "gemini",
		EncryptedAPIKey: ct,
		Nonce:           nonce,
	})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "gem-secret-123" {
		t.Fatalf("got %q, want gem-secret-123", plain)
	}

	// Swapping repo or provider must fail GCM authentication.
	if _, err := c.Decrypt(&domain.RepoCredentialRecord{
		RepoOwner:       "shohei-yamagiwa",
		RepoName:        "other-repo",
		Provider:        "gemini",
		EncryptedAPIKey: ct,
		Nonce:           nonce,
	}); err == nil {
		t.Fatal("expected decryption failure when repo name differs")
	}
	if _, err := c.Decrypt(&domain.RepoCredentialRecord{
		RepoOwner:       "shohei-yamagiwa",
		RepoName:        "mochi",
		Provider:        "openai",
		EncryptedAPIKey: ct,
		Nonce:           nonce,
	}); err == nil {
		t.Fatal("expected decryption failure when provider differs")
	}
}

func TestService_StrictPerRepoResolutionWithoutEnvFallback(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	cipher, err := credentials.NewCipherFromString(testMasterKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := credentials.NewService(st, cipher)
	if err != nil {
		t.Fatal(err)
	}

	// Set global env vars to verify strict mode NEVER falls back to them.
	t.Setenv("GEMINI_API_KEY", "env-gemini-must-not-be-used")
	t.Setenv("OPENAI_API_KEY", "env-openai-must-not-be-used")
	t.Setenv("ANTHROPIC_API_KEY", "env-anthropic-must-not-be-used")

	wf := &domain.Workflow{
		Name: "multi-provider",
		Agents: []domain.RoleConfig{
			{Role: "correctness", LLM: domain.ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash"}},
			{Role: "security", LLM: domain.ModelConfig{Provider: "anthropic", Model: "claude-sonnet"}},
		},
		Synthesizer: domain.SynthesizerConfig{
			LLM: domain.ModelConfig{Provider: "openai", Model: "gpt-5"},
		},
	}

	// Missing all keys -> must fail strictly despite env vars.
	if _, err := svc.ResolveRegistry(ctx, "shohei-yamagiwa", "repo-a", wf); err == nil {
		t.Fatal("expected ResolveRegistry to fail when repo credentials are missing, even with env vars set")
	}

	// Store only gemini for repo-a -> still fails because anthropic and openai are missing.
	if err := svc.Set(ctx, "Shohei-Yamagiwa", "Repo-A", "gemini", "repo-a-gem-key", ""); err != nil {
		t.Fatalf("Set gemini: %v", err)
	}
	if _, err := svc.ResolveRegistry(ctx, "shohei-yamagiwa", "repo-a", wf); err == nil || !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("expected error mentioning missing anthropic key, got %v", err)
	}

	// Store remaining providers for repo-a -> succeeds.
	if err := svc.Set(ctx, "shohei-yamagiwa", "repo-a", "anthropic", "repo-a-anth-key", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Set(ctx, "shohei-yamagiwa", "repo-a", "openai", "repo-a-oai-key", ""); err != nil {
		t.Fatal(err)
	}

	reg, err := svc.ResolveRegistry(ctx, "SHOHEI-YAMAGIWA", "REPO-A", wf)
	if err != nil {
		t.Fatalf("ResolveRegistry(repo-a): %v", err)
	}
	for _, p := range []string{"gemini", "anthropic", "openai"} {
		if _, ok := reg.Get(p); !ok {
			t.Fatalf("expected provider %q in resolved registry", p)
		}
	}

	// repo-b still has no credentials -> must fail strictly!
	if _, err := svc.ResolveRegistry(ctx, "shohei-yamagiwa", "repo-b", wf); err == nil {
		t.Fatal("expected repo-b without credentials to fail")
	}

	// List must return summaries without secrets, Delete removes credential.
	list, err := svc.List(ctx, "shohei-yamagiwa", "repo-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d credentials, want 3", len(list))
	}
	if err := svc.Delete(ctx, "shohei-yamagiwa", "repo-a", "openai"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "shohei-yamagiwa", "repo-a", "openai"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
}
