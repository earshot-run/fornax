package modelrt

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
)

// writeGGUF builds a minimal v3 header with one <arch>.context_length KV.
func writeGGUF(t *testing.T, path string, contextLength uint32) {
	t.Helper()
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(0x46554747))
	binary.Write(&buf, binary.LittleEndian, uint32(3))
	binary.Write(&buf, binary.LittleEndian, uint64(1)) // tensors
	binary.Write(&buf, binary.LittleEndian, uint64(1)) // kv count
	key := "llama.context_length"
	binary.Write(&buf, binary.LittleEndian, uint64(len(key)))
	buf.WriteString(key)
	binary.Write(&buf, binary.LittleEndian, uint32(4)) // uint32
	binary.Write(&buf, binary.LittleEndian, contextLength)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestModelContextReadsTrainedLength(t *testing.T) {
	root := t.TempDir()
	spec := &catalog.Spec{ID: "t", Runtime: catalog.Llama, Model: catalog.Artifact{File: "m.gguf"}}
	writeGGUF(t, paths.ModelFinal(root, spec), 4096)

	trained, ok := ModelContext(root, spec)
	if !ok || trained != 4096 {
		t.Fatalf("ModelContext = %d, %v; want 4096, true", trained, ok)
	}
	if got, note := contextFor(root, spec, 16384); got != 4096 || note == "" {
		t.Fatalf("contextFor lowered to %d (%q), want 4096 with a note", got, note)
	}
	if got, note := contextFor(root, spec, 2048); got != 2048 || note != "" {
		t.Fatalf("contextFor = %d (%q), want 2048 unchanged", got, note)
	}
}

func TestModelContextAbsentWithoutGGUF(t *testing.T) {
	root := t.TempDir()
	spec := &catalog.Spec{ID: "t", Runtime: catalog.Llama, Model: catalog.Artifact{File: "m.gguf"}}
	if _, ok := ModelContext(root, spec); ok {
		t.Fatal("ModelContext claimed a context for a missing file")
	}
	if got, note := contextFor(root, spec, 16384); got != 16384 || note != "" {
		t.Fatalf("contextFor = %d (%q), want the request unchanged", got, note)
	}
}
