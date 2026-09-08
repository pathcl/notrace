package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	orgID      string
	httpClient *http.Client
}

func NewClient(baseURL, token, orgID string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		orgID:   orgID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	return &result, nil
}

func (c *Client) GetTrace(ctx context.Context, traceID string) (*TraceDetail, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/traces/"+traceID)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get trace: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var result TraceDetail
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode trace: %w", err)
	}
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

func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("tempo %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
