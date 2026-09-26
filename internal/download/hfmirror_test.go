package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchHFMirrorWithoutToken(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_TOKEN", "test-private-token")
	payload := []byte("mirror artifact")
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
	if got, _ := os.ReadFile(path); string(got) != string(payload) {
		t.Fatalf("mirror download = %q", got)
	}
}
