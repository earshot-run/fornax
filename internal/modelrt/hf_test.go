package modelrt

import (
	"context"
	"fmt"
	"github.com/earshot-run/fornax/internal/catalog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHFMirrorMetadata(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_TOKEN", "test-private-token")
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token sent to mirror")
		}
		count++
		switch {
		case r.URL.Path == "/api/models/Org/Repo":
			fmt.Fprint(w, `{"siblings":[{"rfilename":"model.gguf"}]}`)
		case r.Method == http.MethodHead && r.URL.Path == "/Org/Repo/resolve/main/model.gguf":
			w.Header().Set("x-linked-etag", strings.Repeat("a", 64))
			w.Header().Set("x-linked-size", "100")
			w.Header().Set("x-repo-commit", "abc123")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	files, err := repoFiles(context.Background(), "Org/Repo")
	if err != nil || len(files) != 1 || files[0] != "model.gguf" {
		t.Fatalf("mirror listing = %v, %v", files, err)
	}
	size, sha, err := hfFileSize(context.Background(), "Org/Repo", "main", files[0])
	if err != nil || size != 100 || count != 2 {
		t.Fatalf("mirror size = %d, %v (requests %d)", size, err, count)
	}
	if sha != strings.Repeat("a", 64) {
		t.Fatalf("mirror digest = %q, want the x-linked-etag", sha)
	}
}

func TestParseHFRefRepoOnly(t *testing.T) {
	for _, arg := range []string{
		"hf:Org/Repo", "Org/Repo", "hf.co/Org/Repo",
		"https://huggingface.co/Org/Repo",
	} {
		repo, rev, file, err := parseHFRef(arg, "")
		if err != nil || repo != "Org/Repo" || rev != "main" || file != "" {
			t.Errorf("parseHFRef(%q) = %q %q %q, %v", arg, repo, rev, file, err)
		}
	}
	repo, rev, file, err := parseHFRef("hf:Org/Repo/Dir/File.gguf@v2", "")
	if repo != "Org/Repo" || rev != "v2" || file != "Dir/File.gguf" || err != nil {
		t.Errorf("parseHFRef deep path = %q %q %q, %v", repo, rev, file, err)
	}
	if _, _, _, err := parseHFRef("hf:NoSlash", ""); err == nil {
		t.Error("a bare word is not an hf ref")
	}
}

func TestPickGGUFFileRanksQuants(t *testing.T) {
	files := []string{
		"Model-F16.gguf", "mmproj-Model-F16.gguf", "Model-Q8_0.gguf",
		"Model-Q4_K_M.gguf", "README.md",
	}
	if got, err := pickGGUFFile("org/repo", files); err != nil || got != "Model-Q4_K_M.gguf" {
		t.Fatalf("pick = %q, %v — want the Q4_K_M", got, err)
	}
	// Quant directories rank by the same list.
	files = []string{"IQ2_XXS/m-00001-of-00002.gguf", "UD-Q4_K_XL/m-00001-of-00002.gguf"}
	if got, _ := pickGGUFFile("org/repo", files); got != "UD-Q4_K_XL/m-00001-of-00002.gguf" {
		t.Fatalf("pick = %q — want the UD-Q4_K_XL dir", got)
	}
	// A lone file needs no ranking.
	if got, _ := pickGGUFFile("org/repo", []string{"only.gguf"}); got != "only.gguf" {
		t.Fatalf("pick = %q", got)
	}
	if _, err := pickGGUFFile("org/repo", []string{"mmproj-x.gguf", "config.json"}); err == nil {
		t.Error("no weights should fail")
	}
	if _, err := pickGGUFFile("org/repo", []string{"a-UD-TQ1_0.gguf", "b-UD-TQ2_0.gguf"}); err == nil {
		t.Error("unrankable multi-file repo should fail")
	}
}

func TestRankedGGUFFilesPreservesQuantOrder(t *testing.T) {
	files := []string{"m-Q3_K_M.gguf", "m-Q8_0.gguf", "m-Q4_K_M.gguf", "mmproj-m.gguf"}
	got, err := rankedGGUFFiles("org/repo", files)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m-Q4_K_M.gguf", "m-Q8_0.gguf", "m-Q3_K_M.gguf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ranked = %v, want %v", got, want)
	}
}

func TestPickGGUFFileFittingPrefersWhatFits(t *testing.T) {
	if MemoryBytes() == 0 {
		t.Skip("machine memory unknown")
	}
	var heads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		heads++
		switch {
		case strings.Contains(r.URL.Path, "Q4_K_M"):
			w.Header().Set("x-linked-size", "9000000000000000000") // far past any RAM
		case strings.Contains(r.URL.Path, "Q3_K_M"):
			w.Header().Set("x-linked-size", "1048576")
		default:
			w.Header().Set("x-linked-size", "1048576")
		}
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)

	files := []string{"m-Q4_K_M.gguf", "m-Q3_K_M.gguf"}
	got, err := pickGGUFFileFitting(context.Background(), "Org/Repo", "main", files)
	if err != nil {
		t.Fatal(err)
	}
	if got != "m-Q3_K_M.gguf" {
		t.Fatalf("picked %q, want the quant that fits", got)
	}
	if heads == 0 {
		t.Fatal("expected the fit check to HEAD the candidates")
	}
}

func TestSplitCompanions(t *testing.T) {
	files := []string{
		"UD-Q4_K_XL/M-00001-of-00003.gguf", "UD-Q4_K_XL/M-00003-of-00003.gguf",
		"UD-Q4_K_XL/M-00002-of-00003.gguf", "Q8_0/M-00001-of-00002.gguf",
		"Q8_0/M-00002-of-00002.gguf",
	}
	got := splitCompanions(files, "UD-Q4_K_XL/M-00001-of-00003.gguf")
	want := []string{"UD-Q4_K_XL/M-00002-of-00003.gguf", "UD-Q4_K_XL/M-00003-of-00003.gguf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("companions = %v, want %v", got, want)
	}
	if got := splitCompanions(files, "Q8_0/M-00001-of-00002.gguf"); len(got) != 1 {
		t.Fatalf("a different dir's split should not mix in: %v", got)
	}
	if got := splitCompanions(files, "single.gguf"); got != nil {
		t.Fatalf("unsplit file gets no companions: %v", got)
	}
}

func TestFindMMProjPrefersSameDir(t *testing.T) {
	files := []string{"other/mmproj-x.gguf", "Q4_K_M/mmproj-m.gguf", "Q4_K_M/m.gguf"}
	if got := findMMProj(files, "Q4_K_M/m.gguf"); got != "Q4_K_M/mmproj-m.gguf" {
		t.Fatalf("mmproj = %q, want the same-dir one", got)
	}
	if got := findMMProj(files, "top/m.gguf"); got != "other/mmproj-x.gguf" {
		t.Fatalf("mmproj = %q, want the repo fallback", got)
	}
	if got := findMMProj(files, "m.gguf"); got != "other/mmproj-x.gguf" {
		t.Fatalf("mmproj = %q", got)
	}
}

func TestInferKind(t *testing.T) {
	cases := []struct {
		repo, file string
		hasProj    bool
		want       string
	}{
		{"org/Qwen3-4B-GGUF", "Qwen3-4B-Q4_K_M.gguf", false, "text"},
		{"org/Qwen3-VL-2B-GGUF", "q.gguf", true, "vision"},
		{"org/ultravox-v0_5-GGUF", "u.gguf", true, "audio"},
		{"org/nomic-embed-text-GGUF", "n.gguf", false, "embed"},
		{"org/Qwen3-Reranker-GGUF", "r.gguf", false, "rerank"},
		{"org/Qwen3-TTS-GGUF", "t.gguf", true, "speech"},
		{"org/Qwen3-VL-2B-GGUF", "q.gguf", false, "text"},
	}
	for _, c := range cases {
		if got := inferKind(c.repo, c.file, c.hasProj); got != c.want {
			t.Errorf("inferKind(%s, %s, %v) = %s, want %s", c.repo, c.file, c.hasProj, got, c.want)
		}
	}
}

// A repo-only pull lists the repo, picks the quant, attaches the projector
// and the split parts, sizes everything, and saves — a repeat resolves
// offline from the saved entry.
func TestEnsureHFEndToEnd(t *testing.T) {
	var listed, heads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/models/") {
			listed++
			w.Write([]byte(`{"siblings":[
				{"rfilename":"VL-2B-F16.gguf"},
				{"rfilename":"VL-2B-Q4_K_M-00001-of-00002.gguf"},
				{"rfilename":"VL-2B-Q4_K_M-00002-of-00002.gguf"},
				{"rfilename":"mmproj-VL-2B-f16.gguf"}
			]}`))
			return
		}
		if r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/resolve/") {
			heads++
			w.Header().Set("x-linked-etag", fmt.Sprintf("\"%064x\"", heads))
			w.Header().Set("x-linked-size", "100")
			w.Header().Set("x-repo-commit", "abc123")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	defer func() { HFHost = old }()
	t.Setenv("FORNAX_HOME", t.TempDir())

	entry, err := ensureHF(context.Background(), "hf:Org/Qwen3-VL-2B-GGUF", "", "", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entry.File != "VL-2B-Q4_K_M-00001-of-00002.gguf" || entry.Kind != "vision" {
		t.Fatalf("entry = %s %s — want the Q4_K_M part 1 as vision", entry.File, entry.Kind)
	}
	if entry.MMProj == nil || entry.MMProj.File != "mmproj-VL-2B-f16.gguf" {
		t.Fatalf("mmproj = %+v", entry.MMProj)
	}
	if len(entry.Companions) != 1 || entry.Companions[0].File != "VL-2B-Q4_K_M-00002-of-00002.gguf" {
		t.Fatalf("companions = %+v", entry.Companions)
	}
	spec := entry.spec()
	if !strings.Contains(spec.URL(&spec.Model), "/resolve/main/") {
		t.Fatalf("a ref without a revision downloads %s, want main", spec.URL(&spec.Model))
	}
	if spec.Kind != catalog.Vision || spec.MMProj == nil || len(spec.Companions) != 1 {
		t.Fatalf("spec lost the vision bits: kind=%v mmproj=%v companions=%d",
			spec.Kind, spec.MMProj, len(spec.Companions))
	}
	// The same ref again: no repo listing, no HEADs.
	listed, heads = 0, 0
	again, err := ensureHF(context.Background(), "hf:Org/Qwen3-VL-2B-GGUF", "", "", "", "", nil, nil)
	if err != nil || again.ID != entry.ID {
		t.Fatalf("repeat resolve = %+v, %v", again, err)
	}
	if listed != 0 || heads != 0 {
		t.Fatalf("repeat resolve hit the network: %d listings, %d heads", listed, heads)
	}
}
