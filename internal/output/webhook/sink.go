package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

const (
	sinkName       = "webhook"
	defaultTimeout = 15 * time.Second
)

// Config configures the outbound HTTP webhook OutputSink.
type Config struct {
	URL        string
	Secret     string
	Headers    map[string]string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Payload is the JSON envelope delivered to outbound webhook receivers.
type Payload struct {
	Event        string          `json:"event"`
	RunID        string          `json:"run_id"`
	WorkflowName string          `json:"workflow_name"`
	TriggerType  string          `json:"trigger_type"`
	DedupKey     string          `json:"dedup_key,omitempty"`
	Status       string          `json:"status"`
	Artifact     json.RawMessage `json:"artifact,omitempty"`
	Error        string          `json:"error,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
}

// Sink implements output.OutputSink (and output.ConfigurableSink) by posting
// workflow artifacts and failure notifications to an HTTP webhook URL.
type Sink struct {
	cfg    Config
	client *http.Client
}

var (
	_ output.OutputSink       = (*Sink)(nil)
	_ output.ConfigurableSink = (*Sink)(nil)
)

// NewSink creates an outbound HTTP webhook OutputSink.
func NewSink(cfg Config) *Sink {
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Sink{
		cfg:    cfg,
		client: hc,
	}
}

// Name returns "webhook".
func (s *Sink) Name() string {
	return sinkName
}

// WithConfig returns a clone applying SinkConfig URL / secret overrides.
func (s *Sink) WithConfig(sc domain.SinkConfig) output.OutputSink {
	nextCfg := s.cfg
	if strings.TrimSpace(sc.URL) != "" {
		nextCfg.URL = strings.TrimSpace(sc.URL)
	}
	if sec, ok := sc.Options["secret"].(string); ok && sec != "" {
		nextCfg.Secret = sec
	}
	return &Sink{
		cfg:    nextCfg,
		client: s.client,
	}
}

// OnRunStarted is a no-op for the outbound webhook sink.
func (s *Sink) OnRunStarted(_ context.Context, _ *domain.TaskContext) error {
	return nil
}

// Publish posts the completed workflow artifact JSON envelope to the configured URL.
func (s *Sink) Publish(ctx context.Context, task *domain.TaskContext, artifact json.RawMessage) error {
	payload := Payload{
		Event:     "workflow.completed",
		Status:    string(domain.RunStatusCompleted),
		Artifact:  artifact,
		Timestamp: time.Now().UTC(),
	}
	fillTaskFields(&payload, task)
	return s.send(ctx, payload)
}

// OnRunFailed posts a workflow failure/cancellation notification if a webhook URL is configured.
func (s *Sink) OnRunFailed(ctx context.Context, task *domain.TaskContext, runErr error) error {
	if strings.TrimSpace(s.cfg.URL) == "" {
		return nil
	}
	status := string(domain.RunStatusFailed)
	if errors.Is(runErr, context.Canceled) {
		status = string(domain.RunStatusCancelled)
	}
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	payload := Payload{
		Event:     "workflow.failed",
		Status:    status,
		Error:     errMsg,
		Timestamp: time.Now().UTC(),
	}
	fillTaskFields(&payload, task)
	return s.send(ctx, payload)
}

func fillTaskFields(p *Payload, task *domain.TaskContext) {
	if task == nil {
		return
	}
	p.RunID = task.RunID
	p.WorkflowName = task.WorkflowName
	p.TriggerType = task.TriggerType
	p.DedupKey = task.DedupKey
}

func (s *Sink) send(ctx context.Context, payload Payload) error {
	targetURL := strings.TrimSpace(s.cfg.URL)
	if targetURL == "" {
		return fmt.Errorf("webhook output sink: url is required")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook output sink marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook output sink build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mochi-webhook-sink/1.0")
	for k, v := range s.cfg.Headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	if s.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(s.cfg.Secret))
		_, _ = mac.Write(body)
		req.Header.Set("X-Mochi-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook output sink post %q: %w", targetURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook output sink %q returned HTTP %d: %s", targetURL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
