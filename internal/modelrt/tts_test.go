package modelrt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
