package modelrt

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
)

func TestSpeechPathsStayInTheCallersDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake engine uses a POSIX shell")
	}
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	work := t.TempDir()
	t.Chdir(work)
	eng := &catalog.EngineSpec{DirName: "fake-tts", TTS: "llama-tts"}
	binary := paths.EngineBinary(root, eng, eng.TTS)
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift ;;
    --tts-speaker-file) voice="$2"; shift ;;
  esac
  shift
done
cat "$voice" > "$out"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("reference.wav", []byte("RIFF test audio"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := &catalog.Spec{ID: "test-tts", MMProj: &catalog.Artifact{File: "projector.gguf"}}
	if err := RunSay(context.Background(), root, eng, spec, "hello", "speech.wav", "reference.wav", "en", 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "RIFF test audio" {
		t.Fatalf("wrong speech: %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "speech.wav")); !os.IsNotExist(err) {
		t.Fatalf("output landed beside engine: %v", err)
	}
}

func TestWarmSpeechReusesTheModelAndReference(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	t.Chdir(t.TempDir())
	key, err := paths.EnsureKey(root)
	if err != nil {
		t.Fatal(err)
	}
	const id = "qwen3-tts-base"
	const wav = "RIFF\x26\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\x02\x00\x00\x00\x00\x00"
	defaultVoice := filepath.Join(root, "voices", id+".wav")
	if err := os.Mkdir(filepath.Dir(defaultVoice), 0700); err != nil {
		t.Fatal(err)
	}
	for path, audio := range map[string]string{defaultVoice: wav, "override.wav": wav[:len(wav)-1] + "\x01"} {
		if err := os.WriteFile(path, []byte(audio), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			fmt.Fprintf(w, `{"data":[{"id":%q,"architecture":{"output_modalities":["audio"]}},{"id":"text-only","architecture":{"input_modalities":["audio"],"output_modalities":["text"]}}]}`, id)
		case "POST /tts":
			var body struct {
				Input  string `json:"input"`
				Lang   string `json:"lang"`
				Voice  []byte `json:"speaker_ref_b64"`
				Frames int    `json:"n_predict"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.Lang != "de" || body.Frames != 90 {
				t.Errorf("wrong speech settings: %+v", body)
			}
			want := wav
			if body.Input == "override" {
				want = wav[:len(wav)-1] + "\x01"
			}
			if string(body.Voice) != want {
				t.Errorf("wrong reference for %q: %q", body.Input, body.Voice)
			}
			w.Header().Set("Content-Type", "audio/wav")
			if body.Input == "truncated" {
				fmt.Fprint(w, wav[:len(wav)-1])
				return
			}
			fmt.Fprint(w, wav)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	spec := &catalog.Spec{ID: id, Kind: catalog.Speech, Port: port}
	eng := &catalog.EngineSpec{DirName: "fake-engine", TTS: "llama-tts"}
	for _, line := range []string{"first sentence", "second sentence", "override"} {
		voice := ""
		if line == "override" {
			voice = "override.wav"
		}
		if err := RunSay(t.Context(), root, eng, spec, line, "speech.wav", voice, "de", 90); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunSay(t.Context(), root, eng, spec, "truncated", "speech.wav", "", "de", 90); err == nil || !strings.Contains(err.Error(), "incomplete WAV") {
		t.Fatalf("expected truncated audio to fail, got %v", err)
	}
	got, err := os.ReadFile("speech.wav")
	if err != nil || string(got) != wav {
		t.Fatalf("speech output changed after failure: %q, %v", got, err)
	}
	if speechServed(&catalog.Spec{ID: "text-only"}, port, key) || speechServed(&catalog.Spec{ID: "other"}, port, key) || speechServed(spec, port, "wrong-key") {
		t.Fatal("selected a server without the requested audio model and key")
	}
}
