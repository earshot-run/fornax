package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Live check against installed models — run with FORNAX_LIVE=1.
func TestSchemaLive(t *testing.T) {
	if os.Getenv("FORNAX_LIVE") == "" {
		t.Skip("set FORNAX_LIVE=1 to run against real models")
	}
	ctx := context.Background()
	spec, eng, err := resolve(ctx, "hf:Qwen/Qwen3-4B-GGUF")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("json_object", func(t *testing.T) {
		out, err := captureStdout(t, func() error {
			return runAskStructured(ctx, spec, eng,
				"What is 2+2? Answer in JSON.", "1", "")
		})
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
			t.Fatalf("reply is not parseable JSON: %v\n%s", err, out)
		}
		t.Logf("reply: %s", out)
	})

	t.Run("json_schema_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "schema.json")
		os.WriteFile(path, []byte(`{
			"type": "object",
			"properties": {"answer": {"type": "string"}},
			"required": ["answer"],
			"additionalProperties": false
		}`), 0o644)
		out, err := captureStdout(t, func() error {
			return runAskStructured(ctx, spec, eng,
				"Name one primary color.", "", path)
		})
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
			t.Fatalf("reply does not fit the schema: %v\n%s", err, out)
		}
		if v.Answer == "" {
			t.Fatalf("schema-conforming reply has an empty answer: %s", out)
		}
		t.Logf("reply: %s", out)
	})

	t.Run("json_schema_stdin", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			io.WriteString(w, `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
			w.Close()
		}()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old }()
		out, err := captureStdout(t, func() error {
			return runAskStructured(ctx, spec, eng,
				"Name one secondary color.", "", "-")
		})
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
			t.Fatalf("reply does not fit the schema: %v\n%s", err, out)
		}
		t.Logf("reply: %s", out)
	})
}

// runAskStructured writes only the payload to stdout; capture it.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}
