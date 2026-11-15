package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	whoutput "github.com/shohei-yamagiwa/mochi/internal/output/webhook"
)

func TestWebhookSink_PublishWithHMACAndOnRunFailed(t *testing.T) {
	secret := "outbound-secret"
	var gotPayloads []whoutput.Payload
	var gotSigs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		gotSigs = append(gotSigs, r.Header.Get("X-Mochi-Signature-256"))

		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if r.Header.Get("X-Mochi-Signature-256") != wantSig {
			t.Fatalf("signature mismatch: got %q want %q", r.Header.Get("X-Mochi-Signature-256"), wantSig)
		}

		var p whoutput.Payload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		gotPayloads = append(gotPayloads, p)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sink := whoutput.NewSink(whoutput.Config{
		Secret:     secret,
		HTTPClient: srv.Client(),
	}).WithConfig(domain.SinkConfig{
		Type: "webhook",
		URL:  srv.URL + "/hook",
	})

	task := &domain.TaskContext{
		RunID:        "run-99",
		WorkflowName: "pr-review",
		TriggerType:  "webhook",
		DedupKey:     "deploy:99",
	}

	if err := sink.Publish(context.Background(), task, json.RawMessage(`{"summary":"ok","verdict":"APPROVE","findings":[]}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := sink.OnRunFailed(context.Background(), task, errors.New("sandbox exploded")); err != nil {
		t.Fatalf("OnRunFailed: %v", err)
	}

	if len(gotPayloads) != 2 {
		t.Fatalf("expected 2 webhook deliveries, got %d", len(gotPayloads))
	}
	if gotPayloads[0].Event != "workflow.completed" || gotPayloads[0].RunID != "run-99" {
		t.Fatalf("unexpected publish payload: %+v", gotPayloads[0])
	}
	if gotPayloads[1].Event != "workflow.failed" || gotPayloads[1].Error != "sandbox exploded" {
		t.Fatalf("unexpected failed payload: %+v", gotPayloads[1])
	}
}
