package main

// `fornax show <model>` — the model card plus a look inside the installed
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

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/gguf"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Rows of embedded metadata to print before folding the rest into "+N more".
const showMetaMax = 15

func cmdShow(args []string) error {
	set := flag.NewFlagSet("show", flag.ExitOnError)
	set.Usage = ui.UsageFunc(set, "usage: fornax show <model>")
	set.Parse(args)
	if set.NArg() != 1 {
		return fmt.Errorf("usage: fornax show <model>")
	}
	spec := modelrt.Model(set.Arg(0))
	if spec == nil {
		return modelrt.UnknownModel(set.Arg(0))
	}
	root := paths.Home()

	row := func(mark, label, value string) {
		fmt.Printf("  %s %s %s\n", mark, ui.Cell(label, 8, ui.Dim), value)
	}
	row(" ", "id", ui.Bold(spec.ID))
	row(" ", "name", spec.Name)
	rt := "llama.cpp"
	switch spec.Runtime {
	case catalog.Kev:
		rt = "kev"
	case catalog.Laya:
		rt = "laya"
	case catalog.Apple:
		rt = "apple fm"
	case catalog.SD:
		rt = "stable-diffusion.cpp"
	}
	row(" ", "kind", kindStyled(spec.Kind)(spec.Kind.String())+ui.Dim(" · "+rt))
	if spec.Repo != "" {
		row(" ", "repo", spec.Repo)
	} else if spec.Model.URL != "" {
		row(" ", "source", ui.Dim(spec.Model.URL))
	}
	if spec.Model.Revision != "" {
		row(" ", "revision", spec.Model.Revision)
	}
	if spec.DType != "" {
		row(" ", "dtype", spec.DType)
	}
	switch partial := paths.ModelPartialBytes(root, spec); {
	case modelrt.Installed(root, spec):
		row(ui.MarkOK(), "status", ui.Green("installed"))
	case partial > 0 && spec.TotalBytes() > 0:
		row(ui.MarkIdle(), "status", ui.Yellow(fmt.Sprintf("%d%% downloaded", partial*100/spec.TotalBytes())))
	default:
		row(ui.MarkIdle(), "status", ui.Dim("not installed"))
	}

	nameWidth := 0
	for _, file := range spec.Files() {
		nameWidth = max(nameWidth, len(file.File))
	}
	for _, file := range spec.Files() {
		mark, state := ui.MarkIdle(), ui.Dim("not installed")
		switch {
		case modelrt.FileInstalled(root, spec, file):
			mark, state = ui.MarkOK(), ui.Green("installed")
		default:
			if part := paths.PartialBytes(paths.PartPath(root, spec, file), file.Bytes); part > 0 {
				state = ui.Yellow(fmt.Sprintf("%d%%", part*100/max(file.Bytes, 1)))
			}
		}
		fmt.Printf("  %s %s %s %s %s\n",
			mark, ui.Cell("file", 8, ui.Dim), ui.Cell(file.File, nameWidth, nil),
			ui.Cell(ui.HumanSize(file.Bytes), 7, nil), state)
	}
	total := ui.HumanSize(spec.TotalBytes())
	if spec.Runtime == catalog.Apple {
		total = "os"
	} else if spec.FitBytes > 0 {
		total += ui.Dim(" · wants ~" + ui.HumanSize(spec.FitBytes) + " RAM")
	}
	row(" ", "total", total)
	port := fmt.Sprintf("%d", spec.Port)
	if spec.Runtime == catalog.SD {
		port += ui.Dim(" — draws, does not serve")
	}
	row(" ", "port", port)
	row(" ", "dir", ui.Dim(paths.ModelDir(root, spec)))

	switch spec.Runtime {
	case catalog.Apple:
		if modelrt.AppleInstalled(root, spec) {
			row(ui.MarkOK(), "artifact", "the model ships in macOS — bridge compiled")
		} else {
			row(ui.MarkIdle(), "artifact", ui.Dim("the model ships in macOS — nothing to download"))
		}
	case catalog.Kev:
		// The tarball unpacks to a checkpoint dir holding head.pt.
		switch ckpt := modelrt.KevCkptDir(root, spec); {
		case ckpt != "":
			row(ui.MarkOK(), "ckpt", filepath.Base(ckpt)+ui.Dim(" — "+ui.HumanSize(modelrt.DirSize(ckpt))+" unpacked"))
		case modelrt.FileInstalled(root, spec, &spec.Model):
			row(ui.MarkIdle(), "ckpt", ui.Dim("tarball installed, checkpoint not unpacked yet"))
		default:
			row(ui.MarkIdle(), "ckpt", ui.Dim("not unpacked yet"))
		}
	}

	file := &spec.Model
	name := strings.ToLower(file.File)
	switch {
	case modelrt.FileInstalled(root, spec, file) && strings.HasSuffix(name, ".gguf"):
		showGGUF(paths.FilePath(root, spec, file))
	case modelrt.FileInstalled(root, spec, file) && strings.HasSuffix(name, ".safetensors"):
		showSafetensors(paths.FilePath(root, spec, file))
	}

	fmt.Println()
	fmt.Printf("  %s\n", ui.Dim(spec.Summary))
	return nil
}

// ── GGUF ────────────────────────────────────────────────────────────────────

func showGGUF(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	version, tensors, kvs, err := gguf.Read(bufio.NewReaderSize(file, 1<<20))
	fmt.Println()
	switch {
	case errors.Is(err, gguf.ErrNotGGUF):
		fmt.Printf("  %s\n", ui.Dim("not a GGUF artifact"))
		return
	case err != nil:
		fmt.Printf("  %s\n", ui.Dim("could not read the GGUF header: "+err.Error()))
		return
	}
	fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("gguf v%d · %d tensors · %d metadata keys", version, tensors, len(kvs))))

	byKey := map[string]string{}
	for _, kv := range kvs {
		byKey[kv.Key] = kv.Val
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
		if strings.HasPrefix(kv.Key, "general.") || strings.HasPrefix(kv.Key, "tokenizer.") ||
			(arch != "" && strings.HasPrefix(kv.Key, arch+".")) {
			pick(kv.Key)
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
				if name, ok := gguf.FileType(n); ok {
					val = name + ui.Dim(fmt.Sprintf(" (ftype %d)", n))
				}
			}
		}
		fmt.Printf("    %s %s\n", ui.Cell(key, width, ui.Dim), val)
	}
	if rest := len(kvs) - len(order); rest > 0 {
		fmt.Printf("    %s\n", ui.Dim(fmt.Sprintf("+ %d more", rest)))
	}
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
	fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("safetensors · %d tensors", tensors)))
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
		fmt.Printf("    %s %v\n", ui.Cell(key, width, ui.Dim), meta[key])
		shown++
	}
	if rest := len(keys) - shown; rest > 0 {
		fmt.Printf("    %s\n", ui.Dim(fmt.Sprintf("+ %d more", rest)))
	}
}
