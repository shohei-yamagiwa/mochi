package main

import (
	"fmt"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// validateRuntimeWorkflow checks local configuration without provisioning sandboxes,
// calling an LLM, or publishing to GitHub. Remote permissions/model availability
// still need an end-to-end smoke test after deployment.
func validateRuntimeWorkflow(wf *domain.Workflow) error {
	ghEnabled := wf.Input.Adapter == "github_pr"
	for _, sink := range wf.Output.Sinks {
		ghEnabled = ghEnabled || sink.Type == "github_pr_review"
	}
	if ghEnabled {
		if _, err := buildGitHubClientFactoryFromEnv(true); err != nil {
			return err
		}
	}
	if err := validateServeAuthConfig(ghEnabled, envAny("GITHUB_WEBHOOK_SECRET"), envAny("MOCHI_TRIGGER_SECRET"), envAny("MOCHI_API_TOKEN"), false); err != nil {
		return err
	}
	if needsSandbox(wf) && envAny("DAYTONA_API_KEY", "DAYTONA_JWT_TOKEN") == "" {
		return fmt.Errorf("sandbox-enabled workflow requires DAYTONA_API_KEY or DAYTONA_JWT_TOKEN")
	}
	providers := map[string][]string{
		"openai":        {"OPENAI_API_KEY"},
		"anthropic":     {"ANTHROPIC_API_KEY"},
		"gemini":        {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		"openai_compat": {"OPENAI_COMPAT_BASE_URL"},
	}
	checkProvider := func(role, provider string) error {
		keys, ok := providers[provider]
		if !ok {
			return fmt.Errorf("%s: unsupported provider %q", role, provider)
		}
		if envAny(keys...) == "" {
			return fmt.Errorf("%s uses %s: set %s", role, provider, strings.Join(keys, " or "))
		}
		return nil
	}
	for _, agent := range wf.Agents {
		if err := checkProvider(agent.Role, agent.LLM.Provider); err != nil {
			return err
		}
	}
	return checkProvider("synthesizer", wf.Synthesizer.LLM.Provider)
}
