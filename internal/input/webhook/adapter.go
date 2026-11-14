package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/input"
)

const adapterName = "webhook"

// TriggerEvent represents a generic HTTP JSON trigger payload.
type TriggerEvent struct {
	WorkflowName string                `json:"workflow_name,omitempty"`
	TriggerType  string                `json:"trigger_type,omitempty"`
	DedupKey     string                `json:"dedup_key,omitempty"`
	RepoURL      string                `json:"repo_url,omitempty"`
	Ref          string                `json:"ref,omitempty"`
	BaseRef      string                `json:"base_ref,omitempty"`
	Snapshot     string                `json:"snapshot,omitempty"`
	Image        string                `json:"image,omitempty"`
	Dockerfile   string                `json:"dockerfile,omitempty"`
	CloneToken   string                `json:"clone_token,omitempty"`
	Workspace    *domain.WorkspaceSpec `json:"workspace,omitempty"`
	Changeset    *domain.ChangesetSpec `json:"changeset,omitempty"`
	Variables    map[string]any        `json:"variables,omitempty"`
	SinkMetadata map[string]any        `json:"sink_metadata,omitempty"`
}

// Adapter implements domain.InputAdapter for generic HTTP JSON webhook triggers.
type Adapter struct {
	defaultWorkflow string
}

var _ domain.InputAdapter = (*Adapter)(nil)

// NewAdapter constructs a generic webhook InputAdapter.
func NewAdapter(defaultWorkflow string) *Adapter {
	return &Adapter{
		defaultWorkflow: strings.TrimSpace(defaultWorkflow),
	}
}

// Name returns "webhook".
func (a *Adapter) Name() string {
	return adapterName
}

// BuildTaskContext normalizes a JSON payload ([]byte, json.RawMessage, *TriggerEvent, or TriggerEvent)
// into a canonical domain.TaskContext.
func (a *Adapter) BuildTaskContext(_ context.Context, rawEvent any) (*domain.TaskContext, error) {
	var ev TriggerEvent
	switch v := rawEvent.(type) {
	case *TriggerEvent:
		if v == nil {
			return nil, fmt.Errorf("webhook input adapter: trigger event must not be nil")
		}
		ev = *v
	case TriggerEvent:
		ev = v
	case []byte:
		if err := json.Unmarshal(v, &ev); err != nil {
			return nil, fmt.Errorf("webhook input adapter: decode json payload: %w", err)
		}
	case json.RawMessage:
		if err := json.Unmarshal(v, &ev); err != nil {
			return nil, fmt.Errorf("webhook input adapter: decode json payload: %w", err)
		}
	default:
		return nil, fmt.Errorf("webhook input adapter: unsupported raw event type %T", rawEvent)
	}

	var ws domain.WorkspaceSpec
	if ev.Workspace != nil {
		ws = *ev.Workspace
	}
	if strings.TrimSpace(ev.RepoURL) != "" {
		ws.RepoURL = strings.TrimSpace(ev.RepoURL)
	}
	if strings.TrimSpace(ev.Ref) != "" {
		ws.Ref = strings.TrimSpace(ev.Ref)
	}
	if strings.TrimSpace(ev.BaseRef) != "" {
		ws.BaseRef = strings.TrimSpace(ev.BaseRef)
	}
	if strings.TrimSpace(ev.Snapshot) != "" {
		ws.Snapshot = strings.TrimSpace(ev.Snapshot)
	}
	if strings.TrimSpace(ev.Image) != "" {
		ws.Image = strings.TrimSpace(ev.Image)
	}
	if strings.TrimSpace(ev.Dockerfile) != "" {
		ws.Dockerfile = strings.TrimSpace(ev.Dockerfile)
	}
	if strings.TrimSpace(ev.CloneToken) != "" {
		ws.CloneToken = strings.TrimSpace(ev.CloneToken)
	}

	var cs *domain.ChangesetSpec
	if ev.Changeset != nil {
		copied := *ev.Changeset
		if copied.Provider == "" {
			copied.Provider = "webhook"
		}
		if len(copied.ChangedFiles) == 0 && strings.TrimSpace(copied.UnifiedDiff) != "" {
			copied.ChangedFiles = input.ParseUnifiedDiff(copied.UnifiedDiff)
		}
		if ws.Ref == "" && copied.HeadSHA != "" {
			ws.Ref = copied.HeadSHA
		}
		if ws.BaseRef == "" && copied.BaseSHA != "" {
			ws.BaseRef = copied.BaseSHA
		}
		cs = &copied
	}

	if ws.RepoURL == "" && cs == nil && len(ev.Variables) == 0 {
		return nil, fmt.Errorf("webhook input adapter: payload must provide repo_url, changeset, or variables")
	}

	wfName := strings.TrimSpace(ev.WorkflowName)
	if wfName == "" {
		wfName = a.defaultWorkflow
	}
	triggerType := strings.TrimSpace(ev.TriggerType)
	if triggerType == "" {
		triggerType = adapterName
	}

	dedupKey := strings.TrimSpace(ev.DedupKey)
	if dedupKey == "" && cs != nil && cs.RepoOwner != "" && cs.RepoName != "" && cs.PRNumber > 0 {
		dedupKey = fmt.Sprintf("webhook:%s/%s:pr:%d", cs.RepoOwner, cs.RepoName, cs.PRNumber)
	}

	var vars map[string]any
	if ev.Variables != nil {
		vars = maps.Clone(ev.Variables)
	}
	var sinkMeta map[string]any
	if ev.SinkMetadata != nil {
		sinkMeta = maps.Clone(ev.SinkMetadata)
	}

	return &domain.TaskContext{
		WorkflowName: wfName,
		TriggerType:  triggerType,
		DedupKey:     dedupKey,
		Workspace:    ws,
		Changeset:    cs,
		Variables:    vars,
		SinkMetadata: sinkMeta,
	}, nil
}
