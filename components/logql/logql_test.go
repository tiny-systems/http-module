package logql

import (
	"strings"
	"testing"

	"github.com/tiny-systems/module/module"
)

func TestParseEntriesCarryStreamLabels(t *testing.T) {
	// Loki nests lines under the stream they came from; flattening them must
	// keep the labels, or an agent cannot say which pod produced a line.
	body := []byte(`{"status":"success","data":{"resultType":"streams","result":[
	  {"stream":{"namespace":"prod","pod":"api-1"},
	   "values":[["1785000000000000000","first"],["1785000001000000000","second"]]}]}}`)
	entries, err := parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Line != "first" {
		t.Errorf("line = %q", entries[0].Line)
	}
	if entries[0].Labels["pod"] != "api-1" {
		t.Errorf("stream labels lost: %+v", entries[0].Labels)
	}
	if !strings.HasPrefix(entries[0].At, "2026-") {
		t.Errorf("nanosecond timestamp not converted: %q", entries[0].At)
	}
}

func TestParseEmptyResult(t *testing.T) {
	entries, err := parse([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want none", len(entries))
	}
}

func TestClassifyRetryability(t *testing.T) {
	// A malformed selector returns 400 every time; a busy backend is worth
	// waiting for.
	if module.IsRetryable(classify(400, []byte("parse error"))) {
		t.Error("400 must not be retryable")
	}
	if !module.IsRetryable(classify(429, []byte("slow down"))) {
		t.Error("429 must be retryable")
	}
	if !module.IsRetryable(classify(503, []byte("unavailable"))) {
		t.Error("503 must be retryable")
	}
}
