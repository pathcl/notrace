package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	orgID      string
	timeout    time.Duration
	verbose    bool
	httpClient *http.Client
}

func NewClient(baseURL, token, orgID string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		orgID:   orgID,
		timeout: timeout,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) SetVerbose(v bool) { c.verbose = v }

func (c *Client) logf(format string, args ...any) {
	if c.verbose {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

func (c *Client) Search(ctx context.Context, q SearchQuery) (*SearchResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/search")
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	if q.Query != "" {
		params.Set("q", q.Query)
	}
	params.Set("start", strconv.FormatInt(q.Start, 10))
	params.Set("end", strconv.FormatInt(q.End, 10))
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	req.URL.RawQuery = params.Encode()

	c.logf("→ GET %s?%s", req.URL.Path, params.Encode())
	start := time.Now()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		c.logf("← %d  elapsed=%s", resp.StatusCode, time.Since(start).Round(time.Millisecond))
		return nil, err
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	c.logf("← %d  traces=%d  inspected=%d  elapsed=%s",
		resp.StatusCode, len(result.Traces), result.Metrics.InspectedTraces,
		time.Since(start).Round(time.Millisecond))

	return &result, nil
}

var getTraceRetryInterval = 2 * time.Second

const getTraceRetryWindow = 30 * time.Second

// GetTrace fetches a full trace by ID. It retries on 404 every 2s for up to
// 30s to handle Tempo's eventual consistency (traces appear in search results
// before the backing block is flushed and queryable).
func (c *Client) GetTrace(ctx context.Context, traceID string) (*TraceDetail, error) {
	deadline := time.Now().Add(getTraceRetryWindow)
	attempt := 0
	for {
		result, err := c.getTrace(ctx, traceID, attempt)
		if err == nil {
			return result, nil
		}
		if err != ErrNotFound || time.Now().After(deadline) {
			return nil, err
		}
		attempt++
		c.logf("  retrying %s (attempt %d, not yet flushed)", traceID, attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(getTraceRetryInterval):
		}
	}
}

func (c *Client) getTrace(ctx context.Context, traceID string, attempt int) (*TraceDetail, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/traces/"+traceID)
	if err != nil {
		return nil, err
	}
	if attempt == 0 {
		c.logf("→ GET /api/traces/%s", traceID)
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get trace: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		c.logf("← %d  elapsed=%s", resp.StatusCode, time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	var result TraceDetail
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode trace: %w", err)
	}
	c.logf("← %d  elapsed=%s", resp.StatusCode, time.Since(start).Round(time.Millisecond))
	return &result, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.orgID != "" {
		req.Header.Set("X-Scope-OrgID", c.orgID)
	}
	return req, nil
}

// ErrNotFound is returned by GetTrace when Tempo responds with 404.
// This is common due to eventual consistency: a traceID appears in search
// results before the underlying block is flushed and queryable.
var ErrNotFound = fmt.Errorf("trace not found")

func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return fmt.Errorf("tempo %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
