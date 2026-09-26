// Package gguf reads the metadata header of a GGUF file — the part before
// the tensor data — without loading anything else.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrNotGGUF means the file does not start with the GGUF magic.
var ErrNotGGUF = errors.New("not a GGUF artifact")

// One metadata entry, its value rendered for display.
type KV struct {
	Key, Val string
}

// Sanity bounds — a corrupt header must not trigger a huge read or alloc.
const (
	maxKeyLen = 4096
	maxStrLen = 1 << 20
	maxKVs    = 1 << 16
	maxArray  = 1 << 32
)

// llama.cpp ftype → quant name (general.file_type means "mostly this type").
var fileTypes = map[int]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 4: "Q4_1+F16",
	7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L",
	14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K",
	19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S", 22: "IQ3_XS", 23: "IQ3_XXS",
	24: "IQ1_XXS", 25: "IQ4_XS", 26: "IQ4_NL", 27: "IQ3_S", 28: "IQ3_M",
	29: "IQ2_S", 30: "IQ2_M", 31: "IQ4_KS", 32: "IQ1_S", 33: "IQ1_M",
	34: "IQ2_K", 35: "IQ2_KS", 36: "IQ3_K", 37: "IQ4_KSS", 38: "IQ5_KS",
	39: "MXFP4",
}

// FileType names a general.file_type value, when it is one llama.cpp knows.
func FileType(n int) (string, bool) {
	name, ok := fileTypes[n]
	return name, ok
}

// Read parses the GGUF header: magic, version, tensor count, then
// metadata_kv_count key/value pairs. Only v2/v3 share this layout — anything
// else stops at the header line. Scalars render to display strings; arrays render as "N items"
// after their payload is read through (never seeked past).
func Read(r *bufio.Reader) (version uint32, tensors uint64, kvs []KV, err error) {
	var magic uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return 0, 0, nil, err
	}
	if magic != 0x46554747 { // "GGUF"
		return 0, 0, nil, ErrNotGGUF
	}
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return 0, 0, nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &tensors); err != nil {
		return 0, 0, nil, err
	}
	var count uint64
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return 0, 0, nil, err
	}
	if version < 2 || version > 3 {
		return version, tensors, nil, nil
	}
	if count > maxKVs {
		return version, tensors, nil, fmt.Errorf("implausible metadata count %d", count)
	}
	for i := uint64(0); i < count; i++ {
		key, err := readString(r, maxKeyLen)
		if err != nil {
			return version, tensors, kvs, err
		}
		var typ uint32
		if err := binary.Read(r, binary.LittleEndian, &typ); err != nil {
			return version, tensors, kvs, err
		}
		val, err := readValue(r, typ)
		if err != nil {
			return version, tensors, kvs, err
		}
		kvs = append(kvs, KV{key, val})
	}
	return version, tensors, kvs, nil
}

func readUint64(r *bufio.Reader) (uint64, error) {
	var n uint64
	err := binary.Read(r, binary.LittleEndian, &n)
	return n, err
}

func readString(r *bufio.Reader, maxLen uint64) (string, error) {
	n, err := readUint64(r)
	if err != nil {
		return "", err
	}
	if n > maxLen {
		return "", fmt.Errorf("string longer than %d bytes", maxLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readValue(r *bufio.Reader, typ uint32) (string, error) {
	switch typ {
	case 0:
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 1:
		var v int8
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 2:
		var v uint16
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 3:
		var v int16
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 4:
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 5:
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 6:
		var v float32
		err := binary.Read(r, binary.LittleEndian, &v)
		return strconv.FormatFloat(float64(v), 'g', -1, 32), err
	case 7:
		var v bool
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 8:
		s, err := readString(r, maxStrLen)
		if err != nil {
			return "", err
		}
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		// Bare for one-liners (arch lookups reuse the value); quoted when
		// it carries newlines, like a chat template.
		if strings.ContainsAny(s, "\n\r\t") {
			return strconv.Quote(s), nil
		}
		return s, nil
	case 9:
		elem, err := readUint32(r)
		if err != nil {
			return "", err
		}
		count, err := readUint64(r)
		if err != nil {
			return "", err
		}
		if err := skipArray(r, elem, count); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d items", count), nil
	case 10:
		var v uint64
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 11:
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return fmt.Sprint(v), err
	case 12:
		var v float64
		err := binary.Read(r, binary.LittleEndian, &v)
		return strconv.FormatFloat(v, 'g', -1, 64), err
	}
	return "", fmt.Errorf("unknown metadata type %d", typ)
}

func readUint32(r *bufio.Reader) (uint32, error) {
	var n uint32
	err := binary.Read(r, binary.LittleEndian, &n)
	return n, err
}

var elemSize = map[uint32]int64{
	0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8,
}

// Fixed-size elements drop wholesale through io.Discard; string elements
// have no fixed size, so every length prefix gets read in turn.
func skipArray(r *bufio.Reader, elem uint32, count uint64) error {
	if elem == 9 {
		return fmt.Errorf("nested arrays are not valid GGUF")
	}
	if count > maxArray {
		return fmt.Errorf("implausible array length %d", count)
	}
	if elem == 8 {
		for i := uint64(0); i < count; i++ {
			n, err := readUint64(r)
			if err != nil {
				return err
			}
			if n > maxStrLen {
				return fmt.Errorf("array string too long")
			}
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return err
			}
		}
		return nil
	}
	size, ok := elemSize[elem]
	if !ok {
		return fmt.Errorf("unknown array element type %d", elem)
	}
	_, err := io.CopyN(io.Discard, r, size*int64(count))
	return err
}
