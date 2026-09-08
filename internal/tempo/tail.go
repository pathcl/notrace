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
	seen := make(map[string]struct{})
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
func (t *Tailer) poll(ctx context.Context, seen map[string]struct{}, fn func([]TraceSearchMetadata) error) error {
	now := time.Now()
	resp, err := t.client.Search(ctx, SearchQuery{
		Query: t.opts.Query,
		Start: now.Add(-t.opts.Lookback).Unix(),
		End:   now.Unix(),
		Limit: 100,
	})
	if err != nil {
		return err
	}

	var fresh []TraceSearchMetadata
	for _, tr := range resp.Traces {
		if _, ok := seen[tr.TraceID]; !ok {
			seen[tr.TraceID] = struct{}{}
			fresh = append(fresh, tr)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	return fn(fresh)
}
