package studio

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/earshot-run/fornax/internal/modelrt"
)

func TestPickPullArgsCarryTheRecipe(t *testing.T) {
	i := slices.IndexFunc(hubPicks, func(p hubPick) bool { return p.Key == "qwen-image-2.1" })
	if i < 0 {
		t.Fatal("qwen-image-2.1 pick is gone")
	}
	got := strings.Join(hubPicks[i].pullArgs(), " ")
	for _, want := range []string{"pull hf:leejet/Qwen-Image-2.1-GGUF/", "--events", "--kind image", "--with vae=hf:", "--with llm=hf:", "--args --cfg-scale 6.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("pull args %q lack %q", got, want)
		}
	}
	for _, p := range hubPicks {
		if p.Bytes <= 0 || !strings.HasPrefix(p.Ref, "hf:") || p.Title == "" || p.Blurb == "" {
			t.Errorf("incomplete pick %+v", p)
		}
		if _, file := splitRef(p.Ref); file == "" {
			t.Errorf("%s: a pick names its exact file, so installed-ness can be matched", p.Key)
		}
	}
}

func TestPopularRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"org/Model-GGUF","downloads":12,"likes":1,"siblings":[{"rfilename":"m.gguf"}]}]`))
	}))
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	t.Cleanup(func() { modelrt.HFHost = old })

	s := testStudio(t)
	h := s.handler(7340)
	rec := studioGet(t, h, "GET", "/api/hub/popular", "127.0.0.1:7340", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"repo":"org/Model-GGUF"`) || !strings.Contains(rec.Body.String(), `"m.gguf"`) {
		t.Fatalf("popular: %d %s", rec.Code, rec.Body)
	}
	if rec := studioGet(t, h, "GET", "/api/hub/popular?kind=nope", "127.0.0.1:7340", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", rec.Code)
	}
}

func TestDownloadKeepsTheKind(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	t.Setenv("FORNAX_ARGS_OUT", out)
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, `
printf '%s\n' "$@" > "$FORNAX_ARGS_OUT"
echo '{"event":"installed","model":"hf-pic","kind":"image"}'
`)
	h := s.handler(7340)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, h, "/api/hub/downloads", `{"ref":"hf:org/Picture","kind":"image"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	if d := waitDownload(t, s, func(d *hubDownload) bool { return d.State != "running" }); d.State != "done" || d.Kind != "image" {
		t.Fatalf("download ended %+v", *d)
	}
	body, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(body), "--kind") || !strings.Contains(string(body), "image") {
		t.Fatalf("pull args %q (%v)", body, err)
	}
	if rec := studioPost(t, h, "/api/hub/downloads", `{"ref":"hf:org/Picture","kind":"rm"}`, cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d %s", rec.Code, rec.Body)
	}
}

func TestSplitRefAndSavedSteps(t *testing.T) {
	if repo, file := splitRef("hf:Comfy-Org/Qwen-Image-2.1/vae/x.safetensors"); repo != "Comfy-Org/Qwen-Image-2.1" || file != "vae/x.safetensors" {
		t.Errorf("splitRef = %q %q", repo, file)
	}
	if n := savedSteps([]string{"--cfg-scale", "1", "--steps", "4"}); n != 4 {
		t.Errorf("savedSteps = %d", n)
	}
	if n := savedSteps(nil); n != 0 {
		t.Errorf("savedSteps(nil) = %d", n)
	}
}

// A stand-in for `fornax pull --events`: two files of progress, then installed.
func fakeFornax(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake fornax is a shell script")
	}
	path := filepath.Join(t.TempDir(), "fornax")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDownloadFollowsPullEvents(t *testing.T) {
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, `
echo '{"event":"progress","label":"engine","done":50,"total":100}'
echo '{"event":"progress","label":"engine","done":100,"total":100}'
echo '{"event":"progress","label":"hf-qwen3-4b","done":400,"total":400}'
echo '{"event":"installed","model":"hf-qwen3-4b","kind":"text"}'
`)
	h := s.handler(7340)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, h, "/api/hub/downloads", `{"key":"qwen3-4b"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	d := waitDownload(t, s, func(d *hubDownload) bool { return d.State != "running" })
	if d.State != "done" || d.Model != "hf-qwen3-4b" || d.Done != d.Total {
		t.Errorf("download ended %+v", *d)
	}
}

func TestDownloadSurfacesPullErrors(t *testing.T) {
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, `
echo '{"event":"error","model":"hf:x/y","message":"no huggingface.co repo \"x/y\""}'
exit 1
`)
	h := s.handler(7340)
	rec := studioPost(t, h, "/api/hub/downloads", `{"ref":"hf:x/y"}`, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	d := waitDownload(t, s, func(d *hubDownload) bool { return d.State != "running" })
	if d.State != "failed" || !strings.Contains(d.Error, "no huggingface.co repo") {
		t.Errorf("download ended %+v", *d)
	}
}

func TestDownloadRejectsUnknownRequests(t *testing.T) {
	s := testStudio(t)
	h := s.handler(7340)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	for _, body := range []string{`{"key":"nope"}`, `{"ref":"/etc/passwd"}`, `{}`} {
		if rec := studioPost(t, h, "/api/hub/downloads", body, cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, rec.Code)
		}
	}
}

func waitDownload(t *testing.T, s *studio, done func(*hubDownload) bool) *hubDownload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.downloads) == 1 && done(s.downloads[0]) {
			d := *s.downloads[0]
			s.mu.Unlock()
			return &d
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("download never finished")
	return nil
}

func studioPost(t *testing.T, h http.Handler, target, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Host = "127.0.0.1:7340"
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHubSortRoutes(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("sort") != "lastModified" {
			t.Errorf("sort = %q", r.URL.Query().Get("sort"))
		}
		w.Write([]byte(`[{"id":"org/New","lastModified":"2026-10-01T00:00:00Z","siblings":[{"rfilename":"new.gguf"}]}]`))
	}))
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	t.Cleanup(func() { modelrt.HFHost = old })
	s := testStudio(t)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	for _, path := range []string{"/api/hub/popular", "/api/hub/search?q=new&"} {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = ""
		}
		rec := studioGet(t, s.handler(7340), "GET", path+separator+"sort=lastModified", "127.0.0.1:7340", cookie)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"updated":"2026-10-01T00:00:00Z"`) {
			t.Fatalf("sorted %s: %d %s", path, rec.Code, rec.Body)
		}
		rec = studioGet(t, s.handler(7340), "GET", path+separator+"sort=wrong", "127.0.0.1:7340", cookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid sort: %d", rec.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d; invalid sorts must fail locally", calls)
	}
}
