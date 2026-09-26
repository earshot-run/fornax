package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func servedIDsServer(t *testing.T, body string) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

func TestServedIDsOpenAIShape(t *testing.T) {
	// llama-server repeats the id inside aliases — report it once.
	server, client := servedIDsServer(t, `{"data":[{"id":"qwen3-4b","aliases":["qwen3-4b"]}],"object":"list"}`)
	ids := servedIDs(client, server.URL, "")
	if len(ids) != 1 || ids[0] != "qwen3-4b" {
		t.Fatalf("servedIDs returned %v", ids)
	}
}

func TestServedIDsKevShape(t *testing.T) {
	server, client := servedIDsServer(t, `{"models":[{"id":"kev-latest","aliases":["jev-latest"]}]}`)
	ids := servedIDs(client, server.URL, "")
	if len(ids) != 2 || !slices.Contains(ids, "kev-latest") || !slices.Contains(ids, "jev-latest") {
		t.Fatalf("servedIDs returned %v", ids)
	}
}

func TestServedIDsDown(t *testing.T) {
	client := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	if ids := servedIDs(client, "http://127.0.0.1:1", "key"); ids != nil {
		t.Fatalf("a dead port should report nothing, got %v", ids)
	}
}
