package jsonfile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

const sinkName = "json_file"

// Sink implements output.OutputSink (and output.ConfigurableSink) by writing
// the formatted JSON artifact to a file path on disk.
type Sink struct {
	path string
}

var (
	_ output.OutputSink       = (*Sink)(nil)
	_ output.ConfigurableSink = (*Sink)(nil)
)

// NewSink creates a JSONFileSink targeting path.
func NewSink(path string) *Sink {
	return &Sink{path: strings.TrimSpace(path)}
}

// Name returns "json_file".
func (s *Sink) Name() string {
	return sinkName
}

// WithConfig returns a clone whose path can be overridden via cfg.Options["path"].
func (s *Sink) WithConfig(cfg domain.SinkConfig) output.OutputSink {
	path := s.path
	if p, ok := cfg.Options["path"].(string); ok && strings.TrimSpace(p) != "" {
		path = strings.TrimSpace(p)
	}
	return &Sink{path: path}
}

// OnRunStarted is a no-op for the JSON file sink.
func (s *Sink) OnRunStarted(_ context.Context, _ *domain.TaskContext) error {
	return nil
}

// Publish formats and writes the artifact JSON to disk atomically via a temp file rename.
func (s *Sink) Publish(_ context.Context, _ *domain.TaskContext, artifact json.RawMessage) error {
	target := strings.TrimSpace(s.path)
	if target == "" {
		return fmt.Errorf("json_file output sink: output path is required")
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, artifact, "", "  "); err != nil {
		formatted.Reset()
		formatted.Write(artifact)
	}
	formatted.WriteByte('\n')

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("json_file create directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".mochi-artifact-*.tmp")
	if err != nil {
		return fmt.Errorf("json_file create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(formatted.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("json_file write %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("json_file close %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("json_file rename to %q: %w", target, err)
	}
	cleanup = false
	return nil
}

// OnRunFailed is a no-op for the JSON file sink.
func (s *Sink) OnRunFailed(_ context.Context, _ *domain.TaskContext, _ error) error {
	return nil
}
