package jsonfile_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output/jsonfile"
)

func TestJSONFileSink_Publish(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "nested", "report.json")
	sink := jsonfile.NewSink("").WithConfig(domain.SinkConfig{
		Type:    "json_file",
		Options: map[string]any{"path": outPath},
	})

	if err := sink.Publish(context.Background(), &domain.TaskContext{RunID: "r1"}, json.RawMessage(`{"verdict":"APPROVE"}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid json written: %v\n%s", err, string(data))
	}
	if parsed["verdict"] != "APPROVE" {
		t.Fatalf("unexpected parsed content: %+v", parsed)
	}
}
