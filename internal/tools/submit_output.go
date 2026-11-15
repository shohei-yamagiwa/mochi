package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/schema"
)

// AgentFinding represents a single issue or observation emitted by a specialist agent.
type AgentFinding struct {
	FilePath       string          `json:"file_path" jsonschema:"Repository-relative file path"`
	StartLine      int             `json:"start_line,omitempty" jsonschema:"1-indexed start line in the new file"`
	EndLine        int             `json:"end_line,omitempty" jsonschema:"1-indexed end line in the new file"`
	Severity       output.Severity `json:"severity" jsonschema:"Severity: critical, high, medium, low, or nit"`
	Category       string          `json:"category" jsonschema:"Category: correctness, security, performance, architecture, or testing"`
	Title          string          `json:"title" jsonschema:"Concise one-line title of the finding"`
	Description    string          `json:"description" jsonschema:"Detailed explanation and impact"`
	SuggestionCode string          `json:"suggestion_code,omitempty" jsonschema:"Exact replacement code snippet for the line range"`
	Confidence     float64         `json:"confidence" jsonschema:"Confidence score between 0.0 and 1.0"`
}

// AgentFindings is the structured output submitted by a specialist agent via submit_output.
type AgentFindings struct {
	Summary       string              `json:"summary" jsonschema:"Concise summary of the specialist agent's analysis"`
	Findings      []AgentFinding      `json:"findings" jsonschema:"List of verified findings discovered by this specialist agent"`
	TestExecution *output.TestSummary `json:"test_execution,omitempty" jsonschema:"Optional build and test execution summary"`
}

// NewAgentFindingsValidator builds a schema.Validator[AgentFindings] with severity enum and confidence bounds.
func NewAgentFindingsValidator() (*schema.Validator[AgentFindings], error) {
	return schema.NewValidator[AgentFindings](func(s *jsonschema.Schema) {
		if findingsProp, ok := s.Properties["findings"]; ok && findingsProp.Items != nil {
			if sevProp, ok := findingsProp.Items.Properties["severity"]; ok {
				sevProp.Enum = []any{
					string(output.SeverityCritical),
					string(output.SeverityHigh),
					string(output.SeverityMedium),
					string(output.SeverityLow),
					string(output.SeverityNit),
				}
			}
			if confProp, ok := findingsProp.Items.Properties["confidence"]; ok {
				confProp.Minimum = jsonschema.Ptr(0.0)
				confProp.Maximum = jsonschema.Ptr(1.0)
			}
		}
	})
}

type submitOutputTool[T any] struct {
	validator    *schema.Validator[T]
	extractCites func(agentRole string, val *T) []domain.FileAccessRecord
}

// NewSubmitOutputTool creates a schema-validated submit_output tool for any output type T.
func NewSubmitOutputTool[T any](
	validator *schema.Validator[T],
	extractCites func(agentRole string, val *T) []domain.FileAccessRecord,
) (Tool, error) {
	if validator == nil {
		return nil, fmt.Errorf("submit_output: validator must not be nil")
	}
	return &submitOutputTool[T]{
		validator:    validator,
		extractCites: extractCites,
	}, nil
}

// NewDefaultSubmitOutputTool creates the default submit_output tool validating AgentFindings.
func NewDefaultSubmitOutputTool() (Tool, error) {
	v, err := NewAgentFindingsValidator()
	if err != nil {
		return nil, err
	}
	return NewSubmitOutputTool[AgentFindings](v, func(agentRole string, val *AgentFindings) []domain.FileAccessRecord {
		if val == nil {
			return nil
		}
		records := make([]domain.FileAccessRecord, 0, len(val.Findings))
		for _, f := range val.Findings {
			cleanPath, err := normalizeRepoPath(f.FilePath, false)
			if err != nil {
				cleanPath = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(f.FilePath), `\`, "/"), "./")
			}
			if cleanPath == "" {
				continue
			}
			var lr *domain.LineRange
			if f.StartLine > 0 {
				endLine := f.EndLine
				if endLine < f.StartLine {
					endLine = f.StartLine
				}
				lr = &domain.LineRange{StartLine: f.StartLine, EndLine: endLine}
			}
			records = append(records, domain.FileAccessRecord{
				AgentRole: agentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessCite,
				LineRange: lr,
			})
		}
		return records
	})
}

func (t *submitOutputTool[T]) Name() string { return "submit_output" }

func (t *submitOutputTool[T]) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Validate and submit the final structured analysis output, terminating the agent loop.",
		Parameters:  t.validator.Schema(),
	}
}

func (t *submitOutputTool[T]) Execute(_ context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	validated, err := t.validator.ValidateJSON(rawArgs)
	if err != nil {
		return &ExecutionResult{
			Output:        fmt.Sprintf("schema validation failed: %v", err),
			ExitCode:      1,
			IsFinalOutput: false,
		}, nil
	}

	normalized, err := json.Marshal(validated)
	if err != nil {
		return nil, fmt.Errorf("marshal validated output: %w", err)
	}

	var cited []domain.FileAccessRecord
	if t.extractCites != nil {
		cited = t.extractCites(execCtx.AgentRole, validated)
	}

	return &ExecutionResult{
		Output:          "output accepted",
		ExitCode:        0,
		AccessedFiles:   cited,
		IsFinalOutput:   true,
		SubmittedOutput: normalized,
	}, nil
}
