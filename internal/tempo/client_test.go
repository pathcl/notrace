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

	client := NewClient(srv.URL, "mytoken", "myorg")
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

	client := NewClient(srv.URL, "", "")
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

	client := NewClient(srv.URL, "", "")
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

	client := NewClient(srv.URL, "", "")
	_, err := client.Search(context.Background(), SearchQuery{
		Start: time.Now().Add(-time.Hour).Unix(),
		End:   time.Now().Unix(),
	})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}
