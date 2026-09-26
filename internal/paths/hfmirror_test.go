package paths

import (
	"strings"
	"testing"
)

func TestHFMirrorURL(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "https://hf-mirror.example")
	got, err := HFMirrorURL("https://huggingface.co/Org/Repo/resolve/abc/file.gguf?download=true")
	if err != nil || got != "https://hf-mirror.example/Org/Repo/resolve/abc/file.gguf?download=true" {
		t.Fatalf("mirror URL = %q, %v", got, err)
	}
	for _, raw := range []string{"https://example.com/file", "https://evil-huggingface.co/file", "http://huggingface.co/file"} {
		got, err := HFMirrorURL(raw)
		if err != nil || got != raw {
			t.Errorf("noncanonical URL %q became %q: %v", raw, got, err)
		}
	}
	for _, bad := range []string{"http://hf-mirror.example", "https://user:pass@mirror.example", "https://mirror.example/path", "https://mirror.example?x=1", "file:///tmp", "https://mirror.example/#frag"} {
		t.Setenv("HF_ENDPOINT", bad)
		if _, err := HFMirrorURL("https://huggingface.co/a"); err == nil || !strings.Contains(err.Error(), "HF_ENDPOINT") {
			t.Errorf("accepted unsafe endpoint %q: %v", bad, err)
		}
	}
	t.Setenv("HF_ENDPOINT", "invalid")
	if got, err := HFMirrorURL("https://example.com/engine.tar.gz"); err != nil || got != "https://example.com/engine.tar.gz" {
		t.Fatalf("non-HF download affected by mirror: %q, %v", got, err)
	}
	t.Setenv("HF_ENDPOINT", "http://127.0.0.1:1234")
	if got, err := HFMirrorURL("https://huggingface.co/a"); err != nil || got != "http://127.0.0.1:1234/a" {
		t.Fatalf("loopback mirror = %q, %v", got, err)
	}
}
