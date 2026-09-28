package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnceFullReadsMessageContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth=%q", got)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3},"timings":{"x":1}}`)
	}))
	defer srv.Close()

	reply, err := OnceFull(context.Background(), srv.URL, "k", "m", []Message{TextMessage("user", "hi")}, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "hello" {
		t.Fatalf("text=%q", reply.Text)
	}
	if reply.Usage == nil || reply.Usage.PromptTokens != 3 {
		t.Fatalf("usage=%+v", reply.Usage)
	}
}

func TestErrorReadsMessage(t *testing.T) {
	resp := &http.Response{
		StatusCode: 400,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad request"}}`)),
	}
	err := Error(resp)
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("err=%v does not carry the status", err)
	}
}

func TestErrorFallsBackToBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader("boom")),
	}
	if err := Error(resp); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestEmbedReadsVectorAndWrappedVector(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"embedding":[0.5,1.5]}]}`,
		`{"data":[{"embedding":[[0.5,1.5]]}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		vec, err := Embed(context.Background(), srv.URL, "k", "m", "text")
		srv.Close()
		if err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if len(vec) != 2 || vec[0] != 0.5 || vec[1] != 1.5 {
			t.Fatalf("body %s: vec=%v", body, vec)
		}
	}
}

func TestGetJSONReturnsNilOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := &http.Client{}
	if got := GetJSON(client, srv.URL+"/ok", "k"); got["status"] != "ok" {
		t.Fatalf("ok: %v", got)
	}
	if got := GetJSON(client, srv.URL+"/bad", "k"); got != nil {
		t.Fatalf("bad: %v, want nil", got)
	}
}

func TestImagePartMIME(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	if err := os.WriteFile(png, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	part, err := ImagePart(png)
	if err != nil {
		t.Fatal(err)
	}
	url := part["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("url=%q", url)
	}
	if _, err := ImagePart(filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("want an error for a non-image")
	}
}

func TestAudioPartFormat(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	if err := os.WriteFile(wav, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	part, err := AudioPart(wav)
	if err != nil {
		t.Fatal(err)
	}
	if part["input_audio"].(map[string]any)["format"] != "wav" {
		t.Fatalf("part=%v", part)
	}
	if _, err := AudioPart(filepath.Join(dir, "noext")); err == nil {
		t.Fatal("want an error for an extensionless file")
	}
}
