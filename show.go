package main

// `fornax show <model>` — the pin card plus a look inside the installed
// artifact: GGUF metadata (arch, params, quant, context), the safetensors
// header for image models, the unpacked checkpoint for kev. Nothing is
// downloaded and no model is loaded.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Rows of embedded metadata to print before folding the rest into "+N more".
const showMetaMax = 15

func cmdShow(args []string) error {
	set := flag.NewFlagSet("show", flag.ExitOnError)
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax show <model>") }
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: fornax show <model>")
	}
	spec := model(set.Arg(0))
	if spec == nil {
		return unknownModel(set.Arg(0))
	}
	root := home()

	row := func(mark, label, value string) {
		fmt.Printf("  %s %s %s\n", mark, cell(label, 8, dim), value)
	}
	row(" ", "id", bold(spec.id))
	row(" ", "name", spec.name)
	rt := "llama.cpp"
	switch spec.rt {
	case runtimeKev:
		rt = "kev"
	case runtimeApple:
		rt = "apple fm"
	case runtimeSD:
		rt = "stable-diffusion.cpp"
	}
	row(" ", "kind", kindStyled(spec.kind)(spec.kind.String())+dim(" · "+rt))
	if spec.repo != "" {
		row(" ", "repo", spec.repo)
	} else if spec.model.url != "" {
		row(" ", "source", dim(spec.model.url))
	}
	if spec.model.revision != "" {
		row(" ", "revision", spec.model.revision)
	}
	if spec.dtype != "" {
		row(" ", "dtype", spec.dtype)
	}
	switch partial := modelPartialBytes(root, spec); {
	case modelInstalled(root, spec):
		row(markOK(), "status", green("installed"))
	case partial > 0 && spec.totalBytes() > 0:
		row(markIdle(), "status", yellow(fmt.Sprintf("%d%% downloaded", partial*100/spec.totalBytes())))
	default:
		row(markIdle(), "status", dim("not installed"))
	}

	nameWidth := 0
	for _, pin := range spec.files() {
		nameWidth = max(nameWidth, len(pin.file))
	}
	for _, pin := range spec.files() {
		mark, state := markIdle(), dim("not installed")
		switch {
		case fileInstalled(root, spec, pin):
			mark, state = markOK(), green("installed")
		default:
			if part := partialBytes(partPath(root, spec, pin), pin.bytes); part > 0 {
				state = yellow(fmt.Sprintf("%d%%", part*100/max(pin.bytes, 1)))
			}
		}
		sha := pin.sha256
		if len(sha) > 12 {
			sha = sha[:12] + "…"
		}
		fmt.Printf("  %s %s %s %s %s %s\n",
			mark, cell("file", 8, dim), cell(pin.file, nameWidth, nil),
			cell(humanSize(pin.bytes), 7, nil), dim("sha256 "+sha), state)
	}
	total := humanSize(spec.totalBytes())
	if spec.rt == runtimeApple {
		total = "os"
	} else if spec.fitBytes > 0 {
		total += dim(" · wants ~" + humanSize(spec.fitBytes) + " RAM")
	}
	row(" ", "total", total)
	port := fmt.Sprintf("%d", spec.port)
	if spec.rt == runtimeSD {
		port += dim(" — draws, does not serve")
	}
	row(" ", "port", port)
	row(" ", "dir", dim(modelDir(root, spec)))

	switch spec.rt {
	case runtimeApple:
		if appleInstalled(root, spec) {
			row(markOK(), "artifact", "the model ships in macOS — bridge compiled")
		} else {
			row(markIdle(), "artifact", dim("the model ships in macOS — nothing to download"))
		}
	case runtimeKev:
		// The pinned tarball unpacks to a checkpoint dir holding head.pt.
		switch ckpt := kevCkptDir(root, spec); {
		case ckpt != "":
			row(markOK(), "ckpt", filepath.Base(ckpt)+dim(" — "+humanSize(dirSize(ckpt))+" unpacked"))
		case fileInstalled(root, spec, &spec.model):
			row(markIdle(), "ckpt", dim("tarball installed, checkpoint not unpacked yet"))
		default:
			row(markIdle(), "ckpt", dim("not unpacked yet"))
		}
	}

	pin := &spec.model
	name := strings.ToLower(pin.file)
	switch {
	case fileInstalled(root, spec, pin) && strings.HasSuffix(name, ".gguf"):
		showGGUF(filePath(root, spec, pin))
	case fileInstalled(root, spec, pin) && strings.HasSuffix(name, ".safetensors"):
		showSafetensors(filePath(root, spec, pin))
	}

	fmt.Println()
	fmt.Printf("  %s\n", dim(spec.summary))
	return nil
}

// ── GGUF ────────────────────────────────────────────────────────────────────

var errNotGGUF = errors.New("not a GGUF artifact")

type ggufKV struct {
	key, val string
}

// Sanity bounds — a corrupt header must not trigger a huge read or alloc.
const (
	ggufMaxKeyLen = 4096
	ggufMaxStrLen = 1 << 20
	ggufMaxKVs    = 1 << 16
	ggufMaxArray  = 1 << 32
)

// llama.cpp ftype → quant name (general.file_type means "mostly this type").
var ggufFTypes = map[int]string{
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

func showGGUF(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	version, tensors, kvs, err := readGGUF(bufio.NewReaderSize(file, 1<<20))
	fmt.Println()
	switch {
	case errors.Is(err, errNotGGUF):
		fmt.Printf("  %s\n", dim("not a GGUF artifact"))
		return
	case err != nil:
		fmt.Printf("  %s\n", dim("could not read the GGUF header: "+err.Error()))
		return
	}
	fmt.Printf("  %s\n", dim(fmt.Sprintf("gguf v%d · %d tensors · %d metadata keys", version, tensors, len(kvs))))

	byKey := map[string]string{}
	for _, kv := range kvs {
		byKey[kv.key] = kv.val
	}
	arch := byKey["general.architecture"]
	var order []string
	seen := map[string]bool{}
	pick := func(key string) {
		if _, ok := byKey[key]; ok && !seen[key] {
			seen[key] = true
			order = append(order, key)
		}
	}
	for _, key := range []string{
		"general.architecture", "general.name", "general.parameter_count",
		"general.file_type", "general.size_label", "general.license",
		"tokenizer.ggml.model",
	} {
		pick(key)
	}
	if arch != "" {
		for _, suffix := range []string{
			".context_length", ".embedding_length", ".block_count",
			".feed_forward_length", ".attention.head_count",
			".attention.head_count_kv", ".attention.sliding_window",
			".rope.freq_base",
		} {
			pick(arch + suffix)
		}
	}
	// Fill the remaining rows with general.*/<arch>.*/tokenizer.* keys in
	// file order; everything left over is counted, not printed.
	for _, kv := range kvs {
		if len(order) >= showMetaMax {
			break
		}
		if strings.HasPrefix(kv.key, "general.") || strings.HasPrefix(kv.key, "tokenizer.") ||
			(arch != "" && strings.HasPrefix(kv.key, arch+".")) {
			pick(kv.key)
		}
	}
	width := 0
	for _, key := range order {
		width = max(width, len(key))
	}
	for _, key := range order {
		val := byKey[key]
		switch key {
		case "general.parameter_count":
			if n, err := strconv.ParseUint(val, 10, 64); err == nil {
				val = humanParams(n)
			}
		case "general.file_type":
			if n, err := strconv.Atoi(val); err == nil {
				if name, ok := ggufFTypes[n]; ok {
					val = name + dim(fmt.Sprintf(" (ftype %d)", n))
				}
			}
		}
		fmt.Printf("    %s %s\n", cell(key, width, dim), val)
	}
	if rest := len(kvs) - len(order); rest > 0 {
		fmt.Printf("    %s\n", dim(fmt.Sprintf("+ %d more", rest)))
	}
}

// The GGUF header: magic, version, tensor count, then metadata_kv_count
// key/value pairs. Only v2/v3 share this layout — anything else stops at the
// header line. Scalars render to display strings; arrays render as "N items"
// after their payload is read through (never seeked past).
func readGGUF(r *bufio.Reader) (version uint32, tensors uint64, kvs []ggufKV, err error) {
	var magic uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return 0, 0, nil, err
	}
	if magic != 0x46554747 { // "GGUF"
		return 0, 0, nil, errNotGGUF
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
	if count > ggufMaxKVs {
		return version, tensors, nil, fmt.Errorf("implausible metadata count %d", count)
	}
	for i := uint64(0); i < count; i++ {
		key, err := ggufString(r, ggufMaxKeyLen)
		if err != nil {
			return version, tensors, kvs, err
		}
		var typ uint32
		if err := binary.Read(r, binary.LittleEndian, &typ); err != nil {
			return version, tensors, kvs, err
		}
		val, err := ggufValue(r, typ)
		if err != nil {
			return version, tensors, kvs, err
		}
		kvs = append(kvs, ggufKV{key, val})
	}
	return version, tensors, kvs, nil
}

func ggufUint64(r *bufio.Reader) (uint64, error) {
	var n uint64
	err := binary.Read(r, binary.LittleEndian, &n)
	return n, err
}

func ggufString(r *bufio.Reader, maxLen uint64) (string, error) {
	n, err := ggufUint64(r)
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

func ggufValue(r *bufio.Reader, typ uint32) (string, error) {
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
		s, err := ggufString(r, ggufMaxStrLen)
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
		elem, err := ggufUint32(r)
		if err != nil {
			return "", err
		}
		count, err := ggufUint64(r)
		if err != nil {
			return "", err
		}
		if err := ggufSkipArray(r, elem, count); err != nil {
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

func ggufUint32(r *bufio.Reader) (uint32, error) {
	var n uint32
	err := binary.Read(r, binary.LittleEndian, &n)
	return n, err
}

var ggufElemSize = map[uint32]int64{
	0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8,
}

// Fixed-size elements drop wholesale through io.Discard; string elements
// have no fixed size, so every length prefix gets read in turn.
func ggufSkipArray(r *bufio.Reader, elem uint32, count uint64) error {
	if elem == 9 {
		return fmt.Errorf("nested arrays are not valid GGUF")
	}
	if count > ggufMaxArray {
		return fmt.Errorf("implausible array length %d", count)
	}
	if elem == 8 {
		for i := uint64(0); i < count; i++ {
			n, err := ggufUint64(r)
			if err != nil {
				return err
			}
			if n > ggufMaxStrLen {
				return fmt.Errorf("array string too long")
			}
			if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
				return err
			}
		}
		return nil
	}
	size, ok := ggufElemSize[elem]
	if !ok {
		return fmt.Errorf("unknown array element type %d", elem)
	}
	_, err := io.CopyN(io.Discard, r, size*int64(count))
	return err
}

// 4_023_456_789 → "4.0B" — parameter counts read better in billions.
func humanParams(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1_000_000)
	}
	return fmt.Sprintf("%d", n)
}

// ── safetensors ─────────────────────────────────────────────────────────────

// The header is one little-endian u64 length then a JSON object:
// tensor name → {dtype, shape, offsets}, plus a "__metadata__" map.
func showSafetensors(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	var lenBuf [8]byte
	if _, err := io.ReadFull(file, lenBuf[:]); err != nil {
		return
	}
	n := binary.LittleEndian.Uint64(lenBuf[:])
	if n == 0 || n > 256<<20 {
		return
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(file, raw); err != nil {
		return
	}
	var header map[string]any
	if json.Unmarshal(raw, &header) != nil {
		return
	}
	meta, _ := header["__metadata__"].(map[string]any)
	tensors := len(header)
	if meta != nil {
		tensors--
	}
	fmt.Println()
	fmt.Printf("  %s\n", dim(fmt.Sprintf("safetensors · %d tensors", tensors)))
	var keys []string
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	width := 0
	for _, key := range keys {
		width = max(width, len(key))
	}
	shown := 0
	for _, key := range keys {
		if shown >= showMetaMax {
			break
		}
		fmt.Printf("    %s %v\n", cell(key, width, dim), meta[key])
		shown++
	}
	if rest := len(keys) - shown; rest > 0 {
		fmt.Printf("    %s\n", dim(fmt.Sprintf("+ %d more", rest)))
	}
}
