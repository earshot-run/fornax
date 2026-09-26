package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
)

func TestAppleText(t *testing.T) {
	if got, err := appleText("hello"); err != nil || got != "hello" {
		t.Fatalf("plain string: %q %v", got, err)
	}
	parts := []any{
		map[string]any{"type": "text", "text": "look at "},
		map[string]any{"type": "text", "text": "this"},
	}
	if got, err := appleText(parts); err != nil || got != "look at this" {
		t.Fatalf("text parts: %q %v", got, err)
	}
	image := []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:…"}}}
	if _, err := appleText(image); err == nil || !strings.Contains(err.Error(), "text-only") {
		t.Fatalf("image part should be rejected, got %v", err)
	}
	if _, err := appleText(42); err == nil {
		t.Fatal("non-string content should be rejected")
	}
}

func TestAppleMuxAuth(t *testing.T) {
	s := &appleServer{spec: &catalog.Spec{ID: "apple-fm"}, key: "test-key"}
	mux := s.mux()

	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("no key should be 401, got %d", denied.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	ok := httptest.NewRecorder()
	mux.ServeHTTP(ok, req)
	if ok.Code != http.StatusOK {
		t.Fatalf("with key should be 200, got %d", ok.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &body); err != nil || len(body.Data) != 1 || body.Data[0].ID != "apple-fm" {
		t.Fatalf("models listing wrong: %v %s", err, ok.Body.String())
	}

	health := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	mux.ServeHTTP(health, req)
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health wrong: %d %s", health.Code, health.Body.String())
	}
}
