package tempo

import (
	"context"
	"fmt"
	"os"
	"time"
)

type TailOptions struct {
	Query    string
	Interval time.Duration
	Lookback time.Duration
	Limit    int
}

type Tailer struct {
	client *Client
	opts   TailOptions
}

func NewTailer(client *Client, opts TailOptions) *Tailer {
	return &Tailer{client: client, opts: opts}
}

// Run polls Tempo on every interval and calls fn with newly seen traces.
// It returns when ctx is cancelled.
func (t *Tailer) Run(ctx context.Context, fn func([]TraceSearchMetadata) error) error {
	seen := make(map[string]time.Time)
	ticker := time.NewTicker(t.opts.Interval)
	defer ticker.Stop()

	// Poll immediately rather than waiting for the first tick.
	if err := t.poll(ctx, seen, fn); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "warn: poll error: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := t.poll(ctx, seen, fn); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "warn: poll error: %v\n", err)
			}
		}
	}
}

// poll queries Tempo for the sliding window and calls fn with traces not yet seen.
// It evicts seen entries older than the lookback window before querying so that
// the dedup map does not suppress traces that re-enter the window.
func (t *Tailer) poll(ctx context.Context, seen map[string]time.Time, fn func([]TraceSearchMetadata) error) error {
	now := time.Now()
	cutoff := now.Add(-t.opts.Lookback)

	for id, seenAt := range seen {
		if seenAt.Before(cutoff) {
			delete(seen, id)
		}
	}

	limit := t.opts.Limit
	if limit <= 0 {
		limit = 100
	}
	resp, err := t.client.Search(ctx, SearchQuery{
		Query: t.opts.Query,
		Start: cutoff.Unix(),
		End:   now.Unix(),
		Limit: limit,
	})
	if err != nil {
		return err
	}

	var fresh []TraceSearchMetadata
	for _, tr := range resp.Traces {
		if _, ok := seen[tr.TraceID]; !ok {
			seen[tr.TraceID] = now
			fresh = append(fresh, tr)
		}
	}

	fmt.Fprintf(os.Stderr, "%s  polled=%d  new=%d  seen=%d\n",
		now.UTC().Format("15:04:05"), len(resp.Traces), len(fresh), len(seen))

	if len(fresh) == 0 {
		return nil
	}
	return fn(fresh)
}
