package output

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/schema"
)

// Verdict represents the overall review recommendation emitted by a code review workflow.
type Verdict string

const (
	VerdictApprove        Verdict = "APPROVE"
	VerdictRequestChanges Verdict = "REQUEST_CHANGES"
	VerdictComment        Verdict = "COMMENT"
)

// Severity represents the impact level of an individual Finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityNit      Severity = "nit"
)

// ReviewReport represents "What to output" for code review workflows.
type ReviewReport struct {
	Summary        string                 `json:"summary" jsonschema:"Executive summary of the code review"`
	Verdict        Verdict                `json:"verdict" jsonschema:"Overall review verdict: APPROVE, REQUEST_CHANGES, or COMMENT"`
	Findings       []Finding              `json:"findings" jsonschema:"Deduplicated list of code review findings"`
	TestExecution  *TestSummary           `json:"test_execution,omitempty" jsonschema:"Summary of build and test execution inside Daytona"`
	Coverage       *domain.CoverageReport `json:"coverage,omitempty" jsonschema:"Deterministic verification of PR diff files vs files inspected by agents"`
	AgentSummaries map[string]string      `json:"agent_summaries,omitempty" jsonschema:"Per-role summary from each specialist agent"`
}

// Finding represents a single deduplicated code review issue or observation.
type Finding struct {
	FilePath       string   `json:"file_path" jsonschema:"Repository-relative file path"`
	StartLine      int      `json:"start_line,omitempty" jsonschema:"1-indexed start line in the new file"`
	EndLine        int      `json:"end_line,omitempty" jsonschema:"1-indexed end line in the new file"`
	Severity       Severity `json:"severity" jsonschema:"Severity: critical, high, medium, low, or nit"`
	Category       string   `json:"category" jsonschema:"Category: correctness, security, performance, architecture, or testing"`
	Title          string   `json:"title" jsonschema:"Concise one-line title of the finding"`
	Description    string   `json:"description" jsonschema:"Detailed explanation and impact"`
	SuggestionCode string   `json:"suggestion_code,omitempty" jsonschema:"Exact replacement code snippet for the line range"`
	Confidence     float64  `json:"confidence" jsonschema:"Confidence score between 0.0 and 1.0"`
	SourceAgents   []string `json:"source_agents" jsonschema:"Specialist agent roles that reported this finding"`
}

// TestSummary records the outcome of build and test commands executed in the sandbox.
type TestSummary struct {
	Ran            bool     `json:"ran" jsonschema:"Whether tests or build commands were executed"`
	Passed         bool     `json:"passed" jsonschema:"Whether all executed test/build commands succeeded"`
	Commands       []string `json:"commands,omitempty" jsonschema:"Commands executed inside the sandbox"`
	Summary        string   `json:"summary" jsonschema:"Concise summary of test and build results"`
	FailedTests    []string `json:"failed_tests,omitempty" jsonschema:"Names of failing tests if any"`
	SkippedReasons []string `json:"skipped_reasons,omitempty" jsonschema:"Reasons tests could not be run if skipped"`
}

// OutputSink formats and delivers workflow artifacts ("How to output").
type OutputSink interface {
	Name() string
	// OnRunStarted is called when a workflow begins (e.g. creates an in-progress GitHub Check Run).
	OnRunStarted(ctx context.Context, task *domain.TaskContext) error
	// Publish formats and delivers the final workflow artifact to the target destination.
	Publish(ctx context.Context, task *domain.TaskContext, artifact json.RawMessage) error
	// OnRunFailed reports unrecoverable workflow errors or cancellations.
	OnRunFailed(ctx context.Context, task *domain.TaskContext, err error) error
}

// NewReviewReportValidator builds a JSON Schema validator for ReviewReport with enum and range constraints.
func NewReviewReportValidator() (*schema.Validator[ReviewReport], error) {
	return schema.NewValidator[ReviewReport](applyReviewReportSchemaConstraints)
}

// NewSynthesizerResponseSchema builds the JSON Schema sent to the Synthesizer LLM, omitting the
// deterministic coverage field (which Mochi computes and injects automatically).
func NewSynthesizerResponseSchema() (*jsonschema.Schema, error) {
	v, err := schema.NewValidator[ReviewReport](func(s *jsonschema.Schema) {
		applyReviewReportSchemaConstraints(s)
		delete(s.Properties, "coverage")
	})
	if err != nil {
		return nil, err
	}
	return v.Schema(), nil
}

func applyReviewReportSchemaConstraints(s *jsonschema.Schema) {
	if verdictProp, ok := s.Properties["verdict"]; ok {
		verdictProp.Enum = []any{string(VerdictApprove), string(VerdictRequestChanges), string(VerdictComment)}
	}
	if findingsProp, ok := s.Properties["findings"]; ok && findingsProp.Items != nil {
		if sevProp, ok := findingsProp.Items.Properties["severity"]; ok {
			sevProp.Enum = []any{
				string(SeverityCritical),
				string(SeverityHigh),
				string(SeverityMedium),
				string(SeverityLow),
				string(SeverityNit),
			}
		}
		if confProp, ok := findingsProp.Items.Properties["confidence"]; ok {
			confProp.Minimum = jsonschema.Ptr(0.0)
			confProp.Maximum = jsonschema.Ptr(1.0)
		}
	}
}

// Registry stores named OutputSink factories or instances.
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]OutputSink
}

// NewRegistry creates an empty OutputSink registry.
func NewRegistry() *Registry {
	return &Registry{
		sinks: make(map[string]OutputSink),
	}
}

// Register adds an OutputSink to the registry under its Name().
func (r *Registry) Register(sink OutputSink) error {
	if sink == nil {
		return fmt.Errorf("cannot register nil output sink")
	}
	name := sink.Name()
	if name == "" {
		return fmt.Errorf("output sink name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sinks[name]; exists {
		return fmt.Errorf("output sink %q already registered", name)
	}
	r.sinks[name] = sink
	return nil
}

// Get retrieves a registered OutputSink by name.
func (r *Registry) Get(name string) (OutputSink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sinks[name]
	return s, ok
}
