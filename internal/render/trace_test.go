package render_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pathcl/notrace/internal/render"
	"github.com/pathcl/notrace/internal/tempo"
)

func sampleTraces() []tempo.TraceSearchMetadata {
	return []tempo.TraceSearchMetadata{
		{
			TraceID:         "abc123def456789012345678",
			RootServiceName: "frontend",
			RootTraceName:   "GET /api/products",
			StartTimeUnixNs: "1725791000000000000",
			DurationMs:      42,
		},
	}
}

func TestRenderTable(t *testing.T) {
	var buf bytes.Buffer
	if err := render.Traces(&buf, sampleTraces(), "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"frontend", "GET /api/products", "42ms", "TRACE ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderNDJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := render.Traces(&buf, sampleTraces(), "json"); err != nil {
		t.Fatalf("render: %v", err)
	}
	dec := json.NewDecoder(&buf)
	var result tempo.TraceSearchMetadata
	if err := dec.Decode(&result); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if result.TraceID != "abc123def456789012345678" {
		t.Errorf("traceID = %q, want %q", result.TraceID, "abc123def456789012345678")
	}
	if result.RootServiceName != "frontend" {
		t.Errorf("rootServiceName = %q, want %q", result.RootServiceName, "frontend")
	}
}

func TestRenderMultipleNDJSON(t *testing.T) {
	traces := []tempo.TraceSearchMetadata{
		{TraceID: "aaa", RootServiceName: "svc-a"},
		{TraceID: "bbb", RootServiceName: "svc-b"},
	}
	var buf bytes.Buffer
	if err := render.Traces(&buf, traces, "json"); err != nil {
		t.Fatalf("render: %v", err)
	}
	dec := json.NewDecoder(&buf)
	count := 0
	for dec.More() {
		var tr tempo.TraceSearchMetadata
		if err := dec.Decode(&tr); err != nil {
			t.Fatalf("decode %d: %v", count, err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("got %d JSON objects, want 2", count)
	}
}

func TestRenderEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := render.Traces(&buf, nil, "table"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output for nil traces, got %q", buf.String())
	}
}

func TestRenderUnknownFormatFallsBackToTable(t *testing.T) {
	var buf bytes.Buffer
	if err := render.Traces(&buf, sampleTraces(), "unknown"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "TRACE ID") {
		t.Error("unknown format should fall back to table")
	}
}
