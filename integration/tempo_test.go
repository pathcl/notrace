//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pathcl/notrace/internal/tempo"
)

func tempoClient(t *testing.T) *tempo.Client {
	t.Helper()
	u := os.Getenv("NOTRACE_TEMPO_URL")
	if u == "" {
		t.Skip("NOTRACE_TEMPO_URL not set — run 'make lab-up' then 'make test-integration'")
	}
	return tempo.NewClient(u, "", "", 10*time.Second)
}

func TestTempoSearchReturnsTraces(t *testing.T) {
	client := tempoClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Search(ctx, tempo.SearchQuery{
		Query: "{}",
		Start: time.Now().Add(-5 * time.Minute).Unix(),
		End:   time.Now().Unix(),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Traces) == 0 {
		t.Error("expected ≥1 trace — is traffic-gen running?")
	}
}

func TestTempoSearchFilterByService(t *testing.T) {
	client := tempoClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Search(ctx, tempo.SearchQuery{
		Query: `{resource.service.name="frontend"}`,
		Start: time.Now().Add(-5 * time.Minute).Unix(),
		End:   time.Now().Unix(),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Traces) == 0 {
		t.Error("expected traces from frontend service")
	}
}

func TestTempoSearchErrorTraces(t *testing.T) {
	client := tempoClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Search(ctx, tempo.SearchQuery{
		Query: `{status=error}`,
		Start: time.Now().Add(-5 * time.Minute).Unix(),
		End:   time.Now().Unix(),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// traffic-gen injects ~15% errors so there should be some
	t.Logf("found %d error traces in last 5m", len(resp.Traces))
}

func TestTempoTailReceivesTraces(t *testing.T) {
	client := tempoClient(t)
	tailer := tempo.NewTailer(client, tempo.TailOptions{
		Query:    "{}",
		Interval: 2 * time.Second,
		Lookback: 30 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	received := make(chan struct{}, 1)
	go func() {
		tailer.Run(ctx, func(traces []tempo.TraceSearchMetadata) error { //nolint:errcheck
			if len(traces) > 0 {
				select {
				case received <- struct{}{}:
				default:
				}
			}
			return nil
		})
	}()

	select {
	case <-received:
		// success
	case <-ctx.Done():
		t.Error("timeout: no traces received within 20s — is traffic-gen running?")
	}
}
