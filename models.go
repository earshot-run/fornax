package main

// The models fornax knows are the built-ins (kev, laya, apple-fm) plus
// whatever `pull hf:…` and `pull ollama:…` saved in custom.json.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
)

func model(id string) *catalog.Spec {
	if spec := catalog.Model(id); spec != nil {
		return spec
	}
	return customSpec(paths.Home(), id)
}

func unknownModel(id string) error {
	var known []string
	var near string
	for _, spec := range allSpecs(paths.Home()) {
		known = append(known, spec.ID)
		if near == "" && (strings.HasPrefix(spec.ID, id) || strings.Contains(spec.ID, id)) {
			near = spec.ID
		}
	}
	if near != "" {
		return fmt.Errorf("unknown model %q — did you mean %q? (known: %s; or `fornax search %s`)", id, near, strings.Join(known, ", "), id)
	}
	return fmt.Errorf("unknown model %q — `fornax search %s` finds it, or pull any GGUF with hf:Org/Repo[/File] (known: %s)", id, id, strings.Join(known, ", "))
}

// What a model does when it does not hold a conversation, and the command
// that does it. An empty verb means the model chats.
func instead(spec *catalog.Spec) (does, command, operands string) {
	switch {
	case spec.Kind == catalog.Decision:
		return "answers typed questions", "judge", `--state "…"`
	case spec.Kind == catalog.Image:
		return "imagines", "imagine", `"prompt"`
	case spec.Kind == catalog.Video:
		return "animates", "animate", `"prompt"`
	case spec.Kind == catalog.Embed:
		return "embeds", "embed", `"text"`
	case spec.Kind == catalog.Speech:
		return "speaks", "say", `"text"`
	case spec.Kind == catalog.Rerank:
		return "ranks documents", "rerank", `"query" <doc…>`
	}
	return "", "", ""
}

// A model is installed when every pinned file is present at its byte count
// and the receipt lists the matching digests. kev models keep their verified
// tarball and the checkpoint it unpacks to.
func modelInstalled(root string, spec *catalog.Spec) bool {
	if spec.Runtime == catalog.Apple {
		return appleInstalled(root, spec)
	}
	dir := paths.ModelDir(root, spec)
	for _, pin := range spec.Files() {
		info, err := os.Stat(paths.FilePath(root, spec, pin))
		if err != nil || info.Size() != pin.Bytes {
			return false
		}
	}
	if spec.Runtime == catalog.Kev && kevCkptDir(root, spec) == "" {
		return false
	}
	return paths.ReadReceipt(filepath.Join(dir, paths.Receipt), spec)
}
