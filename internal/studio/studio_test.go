package studio

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
)

func testStudio(t *testing.T) *studio {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FORNAX_HOME", home)
	s, err := newStudio(home, "esk_local_test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func studioGet(t *testing.T, h http.Handler, method, target, host string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStudioGuard(t *testing.T) {
	s := testStudio(t)
	h := s.handler(7340)
	host := "127.0.0.1:7340"
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }

	if rec := studioGet(t, h, "GET", "/api/models", "evil.example:7340", withCookie); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Host: got %d, want 403", rec.Code)
	}
	if rec := studioGet(t, h, "GET", "/healthz", host, nil); rec.Body.String() != "fornax studio" {
		t.Errorf("healthz answered %q without a key", rec.Body.String())
	}
	if rec := studioGet(t, h, "GET", "/api/models", host, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: got %d, want 401", rec.Code)
	}
	if rec := studioGet(t, h, "GET", "/?key=wrong", host, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: got %d, want 401", rec.Code)
	}

	rec := studioGet(t, h, "GET", "/?key="+s.key, host, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("key link: got %d → %q, want a redirect that drops the key", rec.Code, rec.Header().Get("Location"))
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value != s.key {
		t.Errorf("cookie = %+v, want HttpOnly SameSite=Strict carrying the key", cookie)
	}

	if rec := studioGet(t, h, "GET", "/", host, withCookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fornax studio") {
		t.Errorf("page with cookie: got %d", rec.Code)
	}
	crossSite := func(r *http.Request) { withCookie(r); r.Header.Set("Origin", "http://evil.example") }
	if rec := studioGet(t, h, "POST", "/api/jobs", host, crossSite); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: got %d, want 403", rec.Code)
	}
	if rec := studioGet(t, h, "GET", "/files/library/..%2f..%2fserver.key", host, withCookie); rec.Code != http.StatusNotFound {
		t.Errorf("path traversal: got %d, want 404", rec.Code)
	}
}

func TestStudioProgressFollowsSDOutput(t *testing.T) {
	s := testStudio(t)
	job := &studioJob{studioItem: studioItem{ID: "0123456789abcdef", Kind: "image", Ext: "png", Steps: 8}, State: jobRunning, Total: 8}
	lines := []string{
		"|#############        | 33/128 - 18.64MB/s",
		"|======>              | 1/8 - 25.75s/it",
		"|============>        | 2/8 - 13.62s/it",
		"|==================>  | 3/8 - 2.00it/s",
		"[INFO   ] image.cpp:894  - sampling completed, taking 130.83s",
	}
	for _, line := range lines[:2] {
		s.progress(job, line)
	}
	if job.Phase != "sampling" || job.Step != 1 || job.StepSeconds != 25.75 {
		t.Fatalf("after step 1: phase %q step %d rate %v", job.Phase, job.Step, job.StepSeconds)
	}
	s.progress(job, lines[2])
	if job.StepSeconds != 13.62 {
		t.Errorf("step 2 should replace the warm-up rate, got %v", job.StepSeconds)
	}
	s.progress(job, lines[3])
	if job.Step != 3 || job.StepSeconds != 0.5 {
		t.Errorf("it/s should become 0.5 s/step at step 3, got step %d rate %v", job.Step, job.StepSeconds)
	}
	s.progress(job, lines[4])
	if job.Phase != "decoding" {
		t.Errorf("phase after sampling = %q, want decoding", job.Phase)
	}
}

func TestScanTerminalLinesSplitsCarriageReturns(t *testing.T) {
	scanner := bufio.NewScanner(strings.NewReader("| 1/2 - 3s/it\r| 2/2 - 3s/it\r\n[INFO] done\n"))
	scanner.Split(scanTerminalLines)
	var got []string
	for scanner.Scan() {
		if scanner.Text() != "" {
			got = append(got, scanner.Text())
		}
	}
	if strings.Join(got, "|") != "| 1/2 - 3s/it|| 2/2 - 3s/it|[INFO] done" {
		t.Errorf("lines = %q", got)
	}
}

func TestStudioQueueRunsSavesAndFails(t *testing.T) {
	s := testStudio(t)
	s.generate = func(ctx context.Context, job *studioJob) error {
		switch job.Prompt {
		case "breaks":
			return errors.New("sd-cli exited (exit status 1): out of memory")
		case "hangs":
			<-ctx.Done()
			return ctx.Err()
		}
		s.progress(job, "| 1/2 - 4.00s/it")
		s.progress(job, "| 2/2 - 3.00s/it")
		s.progress(job, "sampling completed")
		s.progress(job, "generate_image completed in 9s")
		return os.WriteFile(s.outputPath(&job.studioItem), []byte("png"), 0o600)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.work(ctx)

	add := func(prompt string) *studioJob {
		job := &studioJob{studioItem: studioItem{ID: newStudioID(), Kind: "image", Ext: "png", Prompt: prompt, Width: 64, Height: 64, Steps: 2, Created: time.Now()}, State: jobQueued, Total: 2}
		s.mu.Lock()
		s.jobs = append(s.jobs, job)
		s.notifyLocked()
		s.mu.Unlock()
		s.wake <- struct{}{}
		return job
	}
	waitFor := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			ok := done()
			s.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	good := add("a lighthouse")
	waitFor("the image to land", func() bool { return s.library == 1 && len(s.jobs) == 0 })
	images, err := s.items("image")
	if err != nil || len(images) != 1 || images[0].ID != good.ID {
		t.Fatalf("gallery = %+v, %v", images, err)
	}
	if images[0].StepSeconds != 3 || images[0].Seconds <= 0 {
		t.Errorf("sidecar lost the measurements: %+v", images[0])
	}

	bad := add("breaks")
	waitFor("the failure", func() bool { return bad.State == jobFailed })
	if !strings.Contains(bad.Error, "out of memory") {
		t.Errorf("error = %q", bad.Error)
	}
	if _, err := os.Stat(s.outputPath(&bad.studioItem)); err == nil {
		t.Error("a failed job left a PNG behind")
	}

	stuck := add("hangs")
	waitFor("the job to start", func() bool { return stuck.State == jobRunning })
	h := s.handler(7340)
	rec := studioGet(t, h, "DELETE", "/api/jobs/"+stuck.ID, "127.0.0.1:7340", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: got %d", rec.Code)
	}
	waitFor("the canceled job to leave the queue", func() bool {
		for _, job := range s.jobs {
			if job == stuck {
				return false
			}
		}
		return true
	})
	if s.library != 1 {
		t.Errorf("a canceled job reached the gallery")
	}
}

func TestStudioRejectsBadRequests(t *testing.T) {
	s := testStudio(t)
	cases := map[string]studioRequest{
		"describe":      {Model: "x", Width: 512, Height: 512, Steps: 20},
		"does not make": {Kind: "sculpture", Model: "x", Prompt: "p"},
		"your image":    {Model: "no-such-model", Prompt: "p", Width: 512, Height: 512, Steps: 20},
		"your speech":   {Kind: "speech", Model: "no-such-model", Prompt: "p"},
	}
	for want, req := range cases {
		if _, err := s.enqueue(req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want it to mention %q", req, err, want)
		}
	}
}

func TestStudioSDKindValidates(t *testing.T) {
	s := testStudio(t)
	cases := map[string]studioRequest{
		"multiple of 16": {Width: 500, Height: 512, Steps: 20},
		"steps":          {Width: 512, Height: 512, Steps: 0},
	}
	for want, req := range cases {
		if err := s.kinds["image"].validate(&req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want it to mention %q", req, err, want)
		}
	}
	video := studioRequest{Width: 512, Height: 512, Steps: 20}
	if err := s.kinds["video"].validate(&video); err == nil || !strings.Contains(err.Error(), "frames") {
		t.Errorf("video without frames: err = %v", err)
	}
}

func TestStudioServesAssets(t *testing.T) {
	s := testStudio(t)
	h := s.handler(7340)
	withCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key}) }
	for _, name := range []string{"core.js", "image.js", "chat.js", "video.js", "voice.js", "models.js", "app.css", "models.css", "mark.svg"} {
		if rec := studioGet(t, h, "GET", "/assets/"+name, "127.0.0.1:7340", withCookie); rec.Code != http.StatusOK {
			t.Errorf("/assets/%s: got %d", name, rec.Code)
		}
	}
	if rec := studioGet(t, h, "GET", "/assets/core.js", "127.0.0.1:7340", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("assets without the key: got %d, want 401", rec.Code)
	}
}

func TestModelLabelUsesTheRepoNameForDerivedIDs(t *testing.T) {
	cases := []struct {
		spec catalog.Spec
		want string
	}{
		{catalog.Spec{ID: "hf-qwen3-tts-12hz-1-7b-base", Name: "hf-qwen3-tts-12hz-1-7b-base", Repo: "ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF"}, "Qwen3-TTS-12Hz-1.7B-Base"},
		{catalog.Spec{ID: "sdxl-lightning", Name: "sdxl-lightning", Repo: "mzwing/SDXL-Lightning-GGUF"}, "sdxl-lightning"},
		{catalog.Spec{ID: "laya", Name: "Laya", Repo: "convaiinnovations/laya"}, "Laya"},
	}
	for _, c := range cases {
		if got := modelLabel(&c.spec); got != c.want {
			t.Errorf("modelLabel(%s) = %q, want %q", c.spec.ID, got, c.want)
		}
	}
}
