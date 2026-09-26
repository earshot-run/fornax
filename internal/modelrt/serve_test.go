package modelrt

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
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

// A model with a projector is two files under one bar: progress must climb
// across both, never restart when the second file begins.
func TestEnsureModelProgressIsCumulative(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	weights, proj := bytes.Repeat([]byte("w"), 3000), bytes.Repeat([]byte("p"), 1000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := weights
		if strings.HasSuffix(r.URL.Path, "proj") {
			body = proj
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer server.Close()
	pin := func(name string, body []byte) catalog.Artifact {
		return catalog.Artifact{File: name + ".gguf", Bytes: int64(len(body)), URL: server.URL + "/" + name}
	}
	projPin := pin("proj", proj)
	spec := &catalog.Spec{ID: "two-files", Runtime: catalog.Llama, Model: pin("weights", weights), MMProj: &projPin}

	var seen []int64
	if err := ensureModel(t.Context(), paths.Home(), spec, func(n int64) { seen = append(seen, n) }); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("progress went backwards: %v", seen)
		}
	}
	if last := seen[len(seen)-1]; last != spec.TotalBytes() {
		t.Fatalf("progress ended at %d, want %d: %v", last, spec.TotalBytes(), seen)
	}
}
