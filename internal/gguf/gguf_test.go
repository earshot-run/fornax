package gguf

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type header struct{ bytes.Buffer }

func (h *header) u32(v uint32) *header { binary.Write(&h.Buffer, binary.LittleEndian, v); return h }
func (h *header) u64(v uint64) *header { binary.Write(&h.Buffer, binary.LittleEndian, v); return h }

func (h *header) str(s string) *header {
	h.u64(uint64(len(s)))
	h.WriteString(s)
	return h
}

func (h *header) kv(key string, typ uint32, value func(*header)) *header {
	h.str(key)
	h.u32(typ)
	value(h)
	return h
}

func read(t *testing.T, h *header) (uint32, uint64, []KV, error) {
	t.Helper()
	return Read(bufio.NewReader(bytes.NewReader(h.Bytes())))
}

func TestReadHeader(t *testing.T) {
	h := &header{}
	h.u32(0x46554747).u32(3).u64(291).u64(2)
	h.kv("general.architecture", 8, func(h *header) { h.str("llama") })
	h.kv("general.file_type", 4, func(h *header) { h.u32(15) })

	version, tensors, kvs, err := read(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 || tensors != 291 {
		t.Fatalf("version=%d tensors=%d, want 3/291", version, tensors)
	}
	want := []KV{{"general.architecture", "llama"}, {"general.file_type", "15"}}
	if len(kvs) != len(want) {
		t.Fatalf("kvs=%v, want %v", kvs, want)
	}
	for i := range want {
		if kvs[i] != want[i] {
			t.Errorf("kv[%d]=%v, want %v", i, kvs[i], want[i])
		}
	}
	if name, ok := FileType(15); !ok || name != "Q4_K_M" {
		t.Errorf("FileType(15)=%q,%v", name, ok)
	}
}

func TestReadRejectsForeignFile(t *testing.T) {
	h := &header{}
	h.u32(0x4d52415a) // "ZARM"
	if _, _, _, err := read(t, h); !errors.Is(err, ErrNotGGUF) {
		t.Fatalf("err=%v, want ErrNotGGUF", err)
	}
}

func TestReadOldVersionStopsAtHeader(t *testing.T) {
	h := &header{}
	h.u32(0x46554747).u32(1).u64(7).u64(3)
	version, tensors, kvs, err := read(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || tensors != 7 || len(kvs) != 0 {
		t.Fatalf("version=%d tensors=%d kvs=%v, want 1/7/empty", version, tensors, kvs)
	}
}

func TestReadArrayRendersCount(t *testing.T) {
	h := &header{}
	h.u32(0x46554747).u32(3).u64(0).u64(2)
	// tokenizer.ggml.tokens: array of two strings.
	h.kv("tokenizer.ggml.tokens", 9, func(h *header) {
		h.u32(8).u64(2).str("a").str("bb")
	})
	// general.quantization_version: array of four uint32.
	h.kv("general.quantization_version", 9, func(h *header) {
		h.u32(4).u64(4).u32(1).u32(2).u32(3).u32(4)
	})
	_, _, kvs, err := read(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 2 || kvs[0].Val != "2 items" || kvs[1].Val != "4 items" {
		t.Fatalf("kvs=%v, want two arrays as \"N items\"", kvs)
	}
}

func TestReadRejectsOversizedCount(t *testing.T) {
	h := &header{}
	h.u32(0x46554747).u32(3).u64(0).u64(maxKVs + 1)
	if _, _, _, err := read(t, h); err == nil {
		t.Fatal("want an error for an implausible metadata count")
	}
}
