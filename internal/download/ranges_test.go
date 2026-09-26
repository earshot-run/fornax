package download

import (
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/earshot-run/fornax/internal/paths"
)

func smallRanges(t *testing.T) {
	t.Helper()
	oldMin, oldRange, oldMax := parallelMin, minRange, maxRanges
	parallelMin, minRange, maxRanges = 1<<10, 1<<12, 4
	t.Cleanup(func() { parallelMin, minRange, maxRanges = oldMin, oldRange, oldMax })
}

func payload(n int) []byte {
	data := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(data)
	return data
}

func TestFetchRangesLandsByteExact(t *testing.T) {
	smallRanges(t)
	data := payload(100_000)
	var ranged atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "f.part")
	var last int64
	if err := Fetch(context.Background(), srv.URL, int64(len(data)), path, func(n int64) { last = n }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) || last != int64(len(data)) {
		t.Fatalf("got %d bytes (progress %d), want %d, equal=%v", len(got), last, len(data), bytes.Equal(got, data))
	}
	if ranged.Load() < 2 {
		t.Errorf("only %d ranged requests; the file should come down in parallel", ranged.Load())
	}
	if _, err := os.Stat(paths.PartRangesPath(path)); err == nil {
		t.Error("the sidecar outlived a finished download")
	}
}

func TestFetchRangesResumesFromTheSidecar(t *testing.T) {
	smallRanges(t)
	data := payload(100_000)
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first pass cuts the last range off halfway, every time.
		if fail.Load() && strings.HasSuffix(r.Header.Get("Range"), "-99999") {
			w.Header().Set("Content-Range", r.Header.Get("Range")[6:]+"/100000")
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "f.part")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Fetch(ctx, srv.URL, int64(len(data)), path, func(int64) {}); err == nil {
		t.Fatal("the first pass should fail on the broken range")
	}
	state, ok := paths.ReadPartRanges(path)
	if !ok {
		t.Fatal("a failed ranged download must leave its sidecar")
	}
	partial := paths.PartialBytes(path, int64(len(data)))
	if partial <= 0 || partial >= int64(len(data)) {
		t.Errorf("PartialBytes = %d after a partial ranged download of %d ranges", partial, len(state.Ranges))
	}
	fail.Store(false)
	if err := Fetch(ctx, srv.URL, int64(len(data)), path, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Fatal("resumed download is not byte-exact")
	}
}

func TestFetchFallsBackWhenRangesAreIgnored(t *testing.T) {
	smallRanges(t)
	data := payload(50_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) // 200, whole body, Range ignored
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "f.part")
	if err := Fetch(context.Background(), srv.URL, int64(len(data)), path, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
		t.Fatal("fallback download is not byte-exact")
	}
}

func TestTokenGoesOnlyToHuggingFace(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_TOKEN", "hf_test")
	for url, want := range map[string]string{
		"https://huggingface.co/org/repo/resolve/main/f.gguf": "Bearer hf_test",
		"https://cas-bridge.xethub.hf.co/x":                   "",
		"https://example.com/f":                               "",
	} {
		req, err := newRequest(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s: Authorization = %q, want %q", url, got, want)
		}
	}
}
