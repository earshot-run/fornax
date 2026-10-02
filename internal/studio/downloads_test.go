package studio

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadJournalRestoresIntentWithoutClaimingInstallation(t *testing.T) {
	s := testStudio(t)
	part := filepath.Join(s.root, "models", "partial", "weights.gguf.part")
	if err := os.MkdirAll(filepath.Dir(part), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, []byte("partial weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.downloads = []*hubDownload{
		{ID: "running", Ref: "hf:org/Weights/Q8_0.gguf", State: "running", Done: 4, Total: 9, Request: downloadRequest{Key: "qwen3-4b", File: "Q8_0.gguf"}},
		{ID: "stale", Ref: "hf:org/Removed", State: "done", Model: "missing-model"},
	}
	if err := s.saveDownloadsLocked(); err != nil {
		t.Fatal(err)
	}
	restored, err := newStudio(s.root, s.key)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.downloads) != 1 {
		t.Fatalf("restored downloads = %+v", restored.downloads)
	}
	d := restored.downloads[0]
	if d.State != "interrupted" || d.Request.File != "Q8_0.gguf" || d.Done != 4 || d.Model != "" || d.cancel != nil {
		t.Fatalf("restored intent = %+v", d)
	}
	data, err := os.ReadFile(part)
	if err != nil || string(data) != "partial weights" {
		t.Fatalf("partial files changed: %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(s.dir, downloadJournal))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("journal is not private: %v", err)
	}
}

func TestGracefulShutdownKeepsResumableDownload(t *testing.T) {
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, `echo '{"event":"progress","label":"weights","done":3,"total":10}'
exec sleep 30
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &hubDownload{ID: "active", Ref: "hf:org/Model", State: "running", Request: downloadRequest{Ref: "hf:org/Model"}, cancel: cancel, files: map[string][2]int64{}}
	s.downloads = []*hubDownload{d}
	done := make(chan struct{})
	go func() { s.runDownload(ctx, d, []string{"pull", d.Ref, "--events"}); close(done) }()
	waitDownload(t, s, func(d *hubDownload) bool { return d.Done == 3 })
	s.interruptDownloads()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("download child did not stop")
	}
	restored, err := newStudio(s.root, s.key)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.downloads) != 1 || restored.downloads[0].State != "interrupted" || restored.downloads[0].Done != 3 {
		t.Fatalf("shutdown lost progress: %+v", restored.downloads)
	}
}

func TestDownloadNeedsInstalledEvent(t *testing.T) {
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, "exit 0\n")
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, s.handler(7340), "/api/hub/downloads", `{"ref":"hf:org/Model"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	d := waitDownload(t, s, func(d *hubDownload) bool { return d.State != "running" })
	if d.State != "failed" || !strings.Contains(d.Error, "without an installed event") {
		t.Fatalf("false completion: %+v", d)
	}
}

func TestJournalFailurePreventsStartingDownload(t *testing.T) {
	s := testStudio(t)
	if err := os.Mkdir(filepath.Join(s.dir, downloadJournal), 0o700); err != nil {
		t.Fatal(err)
	}
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, s.handler(7340), "/api/hub/downloads", `{"key":"qwen3-4b"}`, cookie)
	if rec.Code != http.StatusInternalServerError || len(s.downloads) != 0 {
		t.Fatalf("unsaved download started: %d %s", rec.Code, rec.Body)
	}
}

func TestDismissInterruptedIntentSurvivesRestart(t *testing.T) {
	s := testStudio(t)
	s.downloads = []*hubDownload{{ID: "paused", Ref: "hf:org/Model", State: "interrupted"}}
	if err := s.saveDownloadsLocked(); err != nil {
		t.Fatal(err)
	}
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioGet(t, s.handler(7340), "DELETE", "/api/hub/downloads/paused", "127.0.0.1:7340", cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	restored, err := newStudio(s.root, s.key)
	if err != nil || len(restored.downloads) != 0 {
		t.Fatalf("dismiss reappeared: %v", err)
	}
}

func TestCorruptJournalIsNotOverwritten(t *testing.T) {
	s := testStudio(t)
	path := filepath.Join(s.dir, downloadJournal)
	if err := os.WriteFile(path, []byte("broken journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStudio(s.root, s.key); err == nil {
		t.Fatal("corrupt journal ignored")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "broken journal" {
		t.Fatal("corrupt journal overwritten")
	}
}

func TestAlternatePickQuantRetainsRecipe(t *testing.T) {
	p, err := resolveHubPick("qwen-image-2.1", "alternate-Q4_K_M.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.Ref, "/alternate-Q4_K_M.gguf") || len(p.with) != 3 || p.args == "" || p.Bytes != 0 {
		t.Fatalf("alternate quant lost recipe: %+v", p)
	}
	for _, file := range []string{"../secret.gguf", "mmproj.gguf", "/model.gguf", "weights-00002-of-00003.gguf", "weights.gguf@old"} {
		if _, err := resolveHubPick("qwen-image-2.1", file); err == nil {
			t.Errorf("accepted invalid choice %q", file)
		}
	}
	// Check the actual persisted retry request keeps the exact choice.
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, `echo '{"event":"error","message":"interrupted"}'; exit 1`)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, s.handler(7340), "/api/hub/downloads", `{"key":"qwen-image-2.1","file":"alternate-Q4_K_M.gguf"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	waitDownload(t, s, func(d *hubDownload) bool { return d.State == "failed" })
	data, err := os.ReadFile(filepath.Join(s.dir, downloadJournal))
	if err != nil {
		t.Fatal(err)
	}
	var store downloadStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	if len(store.Downloads) != 1 || store.Downloads[0].Request.File != "alternate-Q4_K_M.gguf" {
		t.Fatal("exact choice lost in journal")
	}
}

func TestOversizedDownloadEventDoesNotHang(t *testing.T) {
	s := testStudio(t)
	s.fornaxPath = fakeFornax(t, "exec dd if=/dev/zero bs=1048576 count=2 2>/dev/null\n")
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	rec := studioPost(t, s.handler(7340), "/api/hub/downloads", `{"ref":"hf:org/Model"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}
	d := waitDownload(t, s, func(d *hubDownload) bool { return d.State != "running" })
	if d.State != "failed" || !strings.Contains(d.Error, "download event stream") {
		t.Fatalf("oversized event: %+v", d)
	}
}
