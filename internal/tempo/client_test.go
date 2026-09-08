package tempo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientAuthHeaders(t *testing.T) {
	var gotAuth, gotOrgID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotOrgID = r.Header.Get("X-Scope-OrgID")
		json.NewEncoder(w).Encode(SearchResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "mytoken", "myorg", 5*time.Second)
	_, err := client.Search(context.Background(), SearchQuery{
		Start: time.Now().Add(-time.Hour).Unix(),
		End:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer mytoken" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer mytoken")
	}
	if gotOrgID != "myorg" {
		t.Errorf("X-Scope-OrgID = %q, want %q", gotOrgID, "myorg")
	}
}

func TestClientNoAuthHeaders(t *testing.T) {
	var gotAuth, gotOrgID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotOrgID = r.Header.Get("X-Scope-OrgID")
		json.NewEncoder(w).Encode(SearchResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)
	_, err := client.Search(context.Background(), SearchQuery{
		Start: time.Now().Add(-time.Hour).Unix(),
		End:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
	if gotOrgID != "" {
		t.Errorf("expected no X-Scope-OrgID header, got %q", gotOrgID)
	}
}

func TestClientSearchQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		json.NewEncoder(w).Encode(SearchResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)
	_, err := client.Search(context.Background(), SearchQuery{
		Query: `{status=error}`,
		Start: time.Now().Add(-time.Hour).Unix(),
		End:   time.Now().Unix(),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != `{status=error}` {
		t.Errorf("q param = %q, want %q", gotQuery, `{status=error}`)
	}
}

func TestClientErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad TraceQL query", http.StatusBadRequest)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)
	_, err := client.Search(context.Background(), SearchQuery{
		Start: time.Now().Add(-time.Hour).Unix(),
		End:   time.Now().Unix(),
	})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// TestGetTraceRetriesOn404 verifies that GetTrace retries on 404 and succeeds
// once the trace becomes available, simulating Tempo's eventual consistency.
func TestGetTraceRetriesOn404(t *testing.T) {
	// Override retry interval to keep the test fast.
	orig := getTraceRetryInterval
	getTraceRetryInterval = 10 * time.Millisecond
	defer func() { getTraceRetryInterval = orig }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(TraceDetail{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)
	_, err := client.GetTrace(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (2 x 404 + 1 success), got %d", calls)
	}
}

// TestGetTraceGivesUpAfterWindow verifies that GetTrace stops retrying and
// returns ErrNotFound once the retry window is exhausted.
func TestGetTraceGivesUpAfterWindow(t *testing.T) {
	orig := getTraceRetryInterval
	getTraceRetryInterval = 10 * time.Millisecond
	defer func() { getTraceRetryInterval = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.GetTrace(ctx, "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestGetTraceNoRetryOnNon404 verifies that non-404 errors (e.g. 500) are
// returned immediately without retrying.
func TestGetTraceNoRetryOnNon404(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "", 5*time.Second)
	_, err := client.GetTrace(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call for non-404 error, got %d", calls)
	}
}
