package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestFetchHFMirrorVerifiedWithoutToken(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_TOKEN", "test-private-token")
	payload := []byte("pinned mirror artifact")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Org/Repo/resolve/abc/model.gguf" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected mirror request %s (authorization present: %v)", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		w.Write(payload)
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	path := filepath.Join(t.TempDir(), "artifact.part")
	if err := Fetch(context.Background(), "https://huggingface.co/Org/Repo/resolve/abc/model.gguf", int64(len(payload)), path, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if err := Verify(path, int64(len(payload)), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
}
