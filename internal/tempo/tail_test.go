package tempo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestTailerDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SearchResponse{
			Traces: []TraceSearchMetadata{
				{TraceID: "abc123", RootServiceName: "svc", RootTraceName: "GET /"},
			},
		})
	}))
	defer srv.Close()

	tailer := NewTailer(NewClient(srv.URL, "", ""), TailOptions{
		Query:    "{}",
		Interval: time.Second,
		Lookback: 30 * time.Second,
	})
	seen := make(map[string]struct{})
	var received []TraceSearchMetadata

	collect := func(traces []TraceSearchMetadata) error {
		received = append(received, traces...)
		return nil
	}

	// First poll — should deliver the trace.
	if err := tailer.poll(context.Background(), seen, collect); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	// Second poll — same trace, should be suppressed.
	if err := tailer.poll(context.Background(), seen, collect); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	if len(received) != 1 {
		t.Errorf("got %d traces, want 1 (dedup should suppress second poll)", len(received))
	}
}

func TestTailerNewTracesDelivered(t *testing.T) {
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		traces := []TraceSearchMetadata{
			{TraceID: "trace-" + strconv.Itoa(call)},
		}
		json.NewEncoder(w).Encode(SearchResponse{Traces: traces})
	}))
	defer srv.Close()

	tailer := NewTailer(NewClient(srv.URL, "", ""), TailOptions{
		Query:    "{}",
		Interval: time.Second,
		Lookback: 30 * time.Second,
	})
	seen := make(map[string]struct{})
	var received []TraceSearchMetadata

	collect := func(traces []TraceSearchMetadata) error {
		received = append(received, traces...)
		return nil
	}

	_ = tailer.poll(context.Background(), seen, collect)
	_ = tailer.poll(context.Background(), seen, collect)

	// Each poll returns a unique traceID, so both should be delivered.
	if len(received) != 2 {
		t.Errorf("got %d traces, want 2", len(received))
	}
}

func TestTailerSlidingWindow(t *testing.T) {
	var capturedStart, capturedEnd int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedStart, _ = strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		capturedEnd, _ = strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		json.NewEncoder(w).Encode(SearchResponse{})
	}))
	defer srv.Close()

	lookback := 30 * time.Second
	tailer := NewTailer(NewClient(srv.URL, "", ""), TailOptions{
		Query:    "{}",
		Interval: time.Second,
		Lookback: lookback,
	})

	before := time.Now().Unix()
	_ = tailer.poll(context.Background(), make(map[string]struct{}), func([]TraceSearchMetadata) error { return nil })
	after := time.Now().Unix()

	if capturedEnd < before || capturedEnd > after+1 {
		t.Errorf("end = %d, want in [%d, %d]", capturedEnd, before, after+1)
	}
	window := capturedEnd - capturedStart
	if window < 29 || window > 31 {
		t.Errorf("window size = %ds, want ~30s", window)
	}
}
