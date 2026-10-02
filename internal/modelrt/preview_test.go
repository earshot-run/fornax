package modelrt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewMatchesMemoryAwarePullAndRevision(t *testing.T) {
	if MemoryBytes() == 0 {
		t.Skip("machine memory unknown")
	}
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	t.Setenv("HF_ENDPOINT", "")
	t.Setenv("HF_TOKEN", "")
	const first = "model-Q5_K_M-00001-of-00002.gguf"
	const second = "model-Q5_K_M-00002-of-00002.gguf"
	seenRevision := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path != "/api/models/org/vision-vl/revision/release" {
				t.Errorf("wrong listing: %s", r.URL.Path)
			}
			seenRevision = true
			fmt.Fprintf(w, `{"siblings":[{"rfilename":%q},{"rfilename":"model-Q4_K_M.gguf"},{"rfilename":%q},{"rfilename":"mmproj.gguf"}]}`, second, first)
			return
		}
		if !strings.Contains(r.URL.Path, "/resolve/release/") {
			t.Errorf("wrong file revision: %s", r.URL.Path)
		}
		size := "64"
		if strings.HasSuffix(r.URL.Path, "model-Q4_K_M.gguf") {
			size = "1000000000000000000"
		}
		if strings.HasSuffix(r.URL.Path, "mmproj.gguf") {
			size = "32"
		}
		w.Header().Set("x-linked-size", size)
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	t.Cleanup(func() { HFHost = old })
	preview, err := PreviewHF(context.Background(), "hf:org/vision-vl@release")
	if err != nil {
		t.Fatal(err)
	}
	if !seenRevision || preview.File != first || preview.Bytes != 160 || len(preview.Files) != 3 || preview.MMProj != "mmproj.gguf" || preview.Revision != "release" {
		t.Fatalf("preview = %+v", preview)
	}
	if _, err := os.Stat(filepath.Join(root, CustomFile)); !os.IsNotExist(err) {
		t.Fatalf("preview saved a model: %v", err)
	}
	entry, err := ensureHF(context.Background(), "hf:org/vision-vl@release", "", "", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entry.File != preview.File || entry.spec().TotalBytes() != preview.Bytes || entry.Ref != preview.Ref {
		t.Fatalf("pull %+v disagrees with preview %+v", entry, preview)
	}
}

func TestRecipePreviewIncludesCompanionsAndArguments(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_ENDPOINT", "")
	t.Setenv("HF_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("preview fetched body: %s", r.Method)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/image.gguf"):
			w.Header().Set("x-linked-size", "11")
		case strings.HasSuffix(r.URL.Path, "/vae.safetensors"):
			w.Header().Set("x-linked-size", "17")
		case strings.HasSuffix(r.URL.Path, "/encoder.gguf"):
			w.Header().Set("x-linked-size", "19")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	t.Cleanup(func() { HFHost = old })
	for _, kind := range []string{"image", "video"} {
		p, err := PreviewHFWithOptions(context.Background(), "hf:org/Image/image.gguf", kind,
			[]Companion{{Flag: "vae", Ref: "hf:org/Files/vae.safetensors"}, {Flag: "t5xxl", Ref: "hf:org/Files/encoder.gguf"}}, []string{"--steps", "4"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind != kind || p.Bytes != 47 || len(p.Files) != 3 || p.Files[1].Flag != "vae" || strings.Join(p.Args, " ") != "--steps 4" {
			t.Fatalf("recipe = %+v", p)
		}
	}
}

func TestReadinessExplainsUnsupportedBackend(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("FORNAX_BACKEND", "unavailable")
	for _, kind := range []string{"text", "image", "video", "speech"} {
		ready := ReadinessForKind(t.TempDir(), kind)
		if ready.Supported || ready.Installed || ready.Reason == "" {
			t.Fatalf("%s: %+v", kind, ready)
		}
	}
}

func TestSplitSelectionRequiresCompleteArchive(t *testing.T) {
	files := []string{"m-Q4_K_M-00001-of-00003.gguf", "m-Q4_K_M-00002-of-00003.gguf"}
	if err := validateSplitFiles(files, files[0]); err == nil {
		t.Fatal("missing shard accepted")
	}
	if err := validateSplitFiles(files, files[1]); err == nil {
		t.Fatal("starting on shard 2 accepted")
	}
	files = append(files, "m-Q4_K_M-00003-of-00003.gguf")
	if err := validateSplitFiles(files, files[0]); err != nil {
		t.Fatal(err)
	}
}
