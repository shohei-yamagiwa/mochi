package domain

import "time"

// RepoCredentialRecord stores an encrypted per-repository LLM provider credential in persistent storage.
type RepoCredentialRecord struct {
	RepoOwner       string
	RepoName        string
	Provider        string
	EncryptedAPIKey []byte
	Nonce           []byte
	BaseURL         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// RepoCredentialSummary exposes non-secret metadata for a configured repository LLM credential.
type RepoCredentialSummary struct {
	RepoOwner string    `json:"repo_owner"`
	RepoName  string    `json:"repo_name"`
	Provider  string    `json:"provider"`
	BaseURL   string    `json:"base_url,omitempty"`
	HasAPIKey bool      `json:"has_api_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
