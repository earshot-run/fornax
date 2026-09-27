package modelrt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A stand-in for sd-server's native API: the job completes on the second
// poll, or never when stall is set.
func fakeSDServer(t *testing.T, stall bool) (*SDServer, *map[string]any, *bool) {
	t.Helper()
	var (
		mu        sync.Mutex
		body      map[string]any
		polls     int
		cancelled bool
	)
	mux := http.NewServeMux()
	for _, route := range []string{"/sdcpp/v1/img_gen", "/sdcpp/v1/vid_gen"} {
		mux.HandleFunc("POST "+route, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			json.NewDecoder(r.Body).Decode(&body)
			body["route"] = r.URL.Path
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"id":"job_1","status":"queued"}`))
		})
	}
	mux.HandleFunc("GET /sdcpp/v1/jobs/job_1", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if stall || polls < 2 {
			w.Write([]byte(`{"status":"generating"}`))
			return
		}
		png := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
		w.Write([]byte(`{"status":"completed","result":{"images":[{"b64_json":"` + png + `"}]}}`))
	})
	mux.HandleFunc("POST /sdcpp/v1/jobs/job_1/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		cancelled = true
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	spec := (&customEntry{ID: "my-image", Kind: "image", File: "sd.safetensors"}).spec()
	return &SDServer{Spec: spec, base: srv.URL, exited: make(chan error), client: srv.Client()}, &body, &cancelled
}

func TestSDServerGenerateWritesTheResult(t *testing.T) {
	s, body, _ := fakeSDServer(t, false)
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.png")
	os.WriteFile(ref, []byte("REF"), 0o600)
	out := filepath.Join(dir, "out.png")
	req := SDRequest{Prompt: "a door", Width: 512, Height: 768, Steps: 8, Seed: 42, Refs: []string{ref}}
	if err := s.Generate(context.Background(), req, out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "PNGDATA" {
		t.Fatalf("wrote %q, want the decoded image", raw)
	}
	got := *body
	if got["route"] != "/sdcpp/v1/img_gen" || got["prompt"] != "a door" || got["width"] != 512.0 || got["seed"] != 42.0 {
		t.Errorf("request = %v", got)
	}
	if steps := got["sample_params"].(map[string]any)["sample_steps"]; steps != 8.0 {
		t.Errorf("sample_steps = %v, want 8", steps)
	}
	if refs := got["ref_images"].([]any); len(refs) != 1 || refs[0] != base64.StdEncoding.EncodeToString([]byte("REF")) {
		t.Errorf("ref_images = %v", refs)
	}
	if _, set := got["negative_prompt"]; set {
		t.Error("an empty negative prompt should keep the server's default")
	}
}

func TestSDServerCancelCancelsTheJob(t *testing.T) {
	s, _, cancelled := fakeSDServer(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := s.Generate(ctx, SDRequest{Prompt: "a door"}, filepath.Join(t.TempDir(), "out.png")); err == nil {
		t.Fatal("a cancelled generation reported success")
	}
	if !*cancelled {
		t.Error("the server job was left running")
	}
}

func TestSDJobBodyRoutesVideo(t *testing.T) {
	spec := (&customEntry{ID: "my-video", Kind: "video", File: "v.gguf"}).spec()
	body, route, err := sdJobBody(spec, SDRequest{Prompt: "a boat", Frames: 33})
	if err != nil {
		t.Fatal(err)
	}
	if route != "vid_gen" || body["video_frames"] != 33 || body["output_format"] != "webm" {
		t.Errorf("video request = %s %v", route, body)
	}
}
