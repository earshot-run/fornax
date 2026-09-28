package modelrt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
)

// A fake sd-server: any submit completes immediately with a PNG.
func imageFixture(t *testing.T) (*sdImageServer, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	png := base64.StdEncoding.EncodeToString([]byte("PNG"))
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sdcpp/v1/img_gen":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			io.WriteString(w, `{"id":"j1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/sdcpp/v1/jobs/j1":
			io.WriteString(w, `{"status":"completed","result":{"b64_json":"`+png+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(native.Close)
	spec := &catalog.Spec{ID: "img", Kind: catalog.Image, Runtime: catalog.SD}
	server := &SDServer{Spec: spec, base: native.URL, client: native.Client()}
	a := &sdImageServer{spec: spec, key: "secret", server: server, dir: t.TempDir(), port: 7340}
	return a, &bodies
}

func postJSON(t *testing.T, srv *httptest.Server, path, key, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestImageGenerationsReturnsB64(t *testing.T) {
	a, bodies := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	resp := postJSON(t, srv, "/v1/images/generations", "secret", `{"prompt":"a fox","size":"512x512","steps":4,"seed":7}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data = %+v", out.Data)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil || string(raw) != "PNG" {
		t.Fatalf("image = %q, %v", raw, err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("native submits = %d", len(*bodies))
	}
	got := (*bodies)[0]
	if got["prompt"] != "a fox" || got["width"] != float64(512) || got["height"] != float64(512) || got["seed"] != float64(7) {
		t.Fatalf("native body = %v", got)
	}
	// A b64 response leaves no file behind.
	entries, _ := os.ReadDir(a.dir)
	if len(entries) != 0 {
		t.Fatalf("dir has %d leftovers", len(entries))
	}
}

func TestImageGenerationsRequiresKey(t *testing.T) {
	a, _ := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	resp := postJSON(t, srv, "/v1/images/generations", "", `{"prompt":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestImageGenerationsRejectsBadSize(t *testing.T) {
	a, _ := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	resp := postJSON(t, srv, "/v1/images/generations", "secret", `{"prompt":"x","size":"huge"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestImageModelsListsTheModel(t *testing.T) {
	a, _ := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"img"`) {
		t.Fatalf("models body = %s", body)
	}
}

func TestImageEditsPassesReference(t *testing.T) {
	a, bodies := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	form.WriteField("prompt", "make it blue")
	part, _ := form.CreateFormFile("image", "in.png")
	part.Write([]byte("PNG"))
	form.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(*bodies) != 1 {
		t.Fatalf("native submits = %d", len(*bodies))
	}
	refs, ok := (*bodies)[0]["ref_images"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("ref_images = %v", (*bodies)[0]["ref_images"])
	}
}

func TestImageURLResponseIsServed(t *testing.T) {
	a, _ := imageFixture(t)
	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	resp := postJSON(t, srv, "/v1/images/generations", "secret", `{"prompt":"x","response_format":"url"}`)
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data = %+v", out.Data)
	}
	name := filepath.Base(out.Data[0].URL)
	if _, err := os.Stat(filepath.Join(a.dir, name)); err != nil {
		t.Fatalf("url file missing: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/files/"+name, nil)
	req.Header.Set("Authorization", "Bearer secret")
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	body, _ := io.ReadAll(got.Body)
	if got.StatusCode != http.StatusOK || string(body) != "PNG" {
		t.Fatalf("files status %d body %q", got.StatusCode, body)
	}
}

func TestParseImageSize(t *testing.T) {
	if w, h, err := parseImageSize("512x768"); err != nil || w != 512 || h != 768 {
		t.Fatalf("512x768 = %d,%d,%v", w, h, err)
	}
	for _, bad := range []string{"", "huge", "512", "16x16", "5000x5000"} {
		if _, _, err := parseImageSize(bad); err == nil {
			t.Errorf("parseImageSize(%q) accepted it", bad)
		}
	}
}

func TestServedImageFalseWhenNothingServing(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	spec := &catalog.Spec{ID: "img", Kind: catalog.Image, Runtime: catalog.SD, Port: 7399}
	handled, err := ServedImage(context.Background(), spec, SDRequest{Prompt: "x"}, filepath.Join(t.TempDir(), "o.png"))
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("handled = true with nothing serving")
	}
}
