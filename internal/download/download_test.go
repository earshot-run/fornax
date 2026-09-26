package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseContentRange(t *testing.T) {
	cr, err := parseContentRange("bytes 100-199/1024")
	if err != nil || cr.start != 100 || cr.end != 199 || cr.total != 1024 {
		t.Fatalf("parseContentRange = %+v, %v", cr, err)
	}
	for _, bad := range []string{"bytes 100-/1024", "100-199/1024", "bytes a-b/c", ""} {
		if _, err := parseContentRange(bad); err == nil {
			t.Errorf("parseContentRange(%q) succeeded, want error", bad)
		}
	}
}

func TestFetchResumesAPartDownload(t *testing.T) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		var start int64
		if rangeHeader != "" {
			n, _ := fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if n != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		if start > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)-int(start)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
	defer server.Close()

	root := t.TempDir()
	part := filepath.Join(root, "model.gguf.part")
	// An interrupted first attempt left half the bytes behind.
	os.WriteFile(part, payload[:2048], 0o600)

	if err := Fetch(context.Background(), server.URL, int64(len(payload)), part, func(int64) {}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := os.ReadFile(part)
	if len(got) != len(payload) {
		t.Fatalf("fetched %d bytes, want %d", len(got), len(payload))
	}
	sum := sha256.Sum256(payload)
	if err := Verify(part, int64(len(payload)), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsTamperedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model.gguf")
	os.WriteFile(path, []byte("tampered"), 0o600)
	sum := sha256.Sum256([]byte("honest!!"))
	err := Verify(path, 8, hex.EncodeToString(sum[:]))
	if err == nil {
		t.Fatal("verify accepted a bad digest")
	}
	if _, stat := os.Stat(path); !os.IsNotExist(stat) {
		t.Fatal("a failed artifact was left behind")
	}
}
