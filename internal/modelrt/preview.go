package modelrt

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/paths"
)

// RuntimeReadiness describes a compatible engine build, not a promise that
// arbitrary weights or every context size will run on this machine.
type RuntimeReadiness struct {
	Supported bool   `json:"supported"`
	Engine    string `json:"engine,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Installed bool   `json:"installed"`
	Reason    string `json:"reason"`
}

// ReadinessForKind probes the same engine selection used by Resolve, without
// saving a model, installing anything, or making network requests.
func ReadinessForKind(root, kind string) RuntimeReadiness {
	var eng *catalog.EngineSpec
	var err error
	if kind == "image" || kind == "video" {
		eng, err = SDEngine()
	} else {
		eng, err = LlamaEngine()
	}
	if err != nil {
		return RuntimeReadiness{Reason: err.Error()}
	}
	ready := RuntimeReadiness{Supported: true, Engine: eng.Name, Backend: string(eng.Backend), Installed: paths.EngineInstalled(root, eng)}
	ready.Reason = "Compatible engine build available; downloaded on first use."
	if ready.Installed {
		ready.Reason = "Engine installed. Model compatibility still depends on its architecture and files."
	}
	return ready
}

// PreviewFile is one artifact in the complete download, including shards,
// projectors and the recipe's extra files.
type PreviewFile struct {
	Ref   string `json:"ref"`
	File  string `json:"file"`
	Role  string `json:"role"`
	Flag  string `json:"flag,omitempty"`
	Bytes int64  `json:"bytes"`
}

type Preview struct {
	Repo      string           `json:"repo"`
	Revision  string           `json:"revision"`
	Ref       string           `json:"ref"`
	File      string           `json:"file"`
	MMProj    string           `json:"mmproj,omitempty"`
	Kind      string           `json:"kind"`
	Bytes     int64            `json:"bytes"`
	Files     []PreviewFile    `json:"files"`
	Args      []string         `json:"args,omitempty"`
	Readiness RuntimeReadiness `json:"readiness"`
	FreeDisk  *int64           `json:"free_disk,omitempty"`
}

// PreviewHF resolves exactly as a repo-only pull does, including memory-aware
// quant selection, without downloading weights or saving custom.json.
func PreviewHF(ctx context.Context, ref string) (*Preview, error) {
	return PreviewHFWithOptions(ctx, ref, "", nil, nil)
}

// PreviewHFWithOptions includes the same kind override, companion refs and
// engine arguments used by a studio recipe's pull. It only reads metadata.
func PreviewHFWithOptions(ctx context.Context, ref, kind string, with []Companion, args []string) (*Preview, error) {
	repo, revision, file, err := parseHFRef(ref, "")
	if err != nil {
		return nil, err
	}
	switch kind {
	case "", "text", "vision", "audio", "embed", "rerank", "speech", "image", "video":
	default:
		return nil, fmt.Errorf("unknown model kind %q", kind)
	}
	var files []string
	if file == "" || (kind != "image" && kind != "video") || splitPartPattern.MatchString(filepath.Base(file)) {
		files, err = repoFilesRevision(ctx, repo, revision)
		if err != nil {
			return nil, err
		}
	}
	if file == "" {
		file, err = pickGGUFFileFitting(ctx, repo, revision, files)
		if err != nil {
			return nil, err
		}
	} else if len(files) > 0 && !slices.Contains(files, file) {
		return nil, fmt.Errorf("%s has no file %q at revision %s", repo, file, revision)
	}
	if err := validateSplitFiles(files, file); err != nil {
		return nil, err
	}
	if kind == "" {
		kind = inferKind(repo, file, findMMProj(files, file) != "")
	}
	p := &Preview{Repo: repo, Revision: revision, Ref: canonicalHFRef(repo, revision, file), File: file, Kind: kind, Args: args,
		Readiness: ReadinessForKind(paths.Home(), kind)}
	add := func(repo, revision, file, role, flag string) error {
		size, _, err := hfFileSize(ctx, repo, revision, file)
		if err != nil {
			return err
		}
		p.Files = append(p.Files, PreviewFile{Ref: canonicalHFRef(repo, revision, file), File: file, Role: role, Flag: flag, Bytes: size})
		p.Bytes += size
		return nil
	}
	if err := add(repo, revision, file, "weights", ""); err != nil {
		return nil, err
	}
	if kind == "vision" || kind == "audio" || kind == "speech" {
		p.MMProj = findMMProj(files, file)
		if p.MMProj == "" {
			return nil, fmt.Errorf("--kind %s needs a projector — %s has no mmproj-*.gguf", kind, repo)
		}
		if err := add(repo, revision, p.MMProj, "projector", "mmproj"); err != nil {
			return nil, err
		}
	}
	for _, part := range splitCompanions(files, file) {
		if err := add(repo, revision, part, "shard", ""); err != nil {
			return nil, err
		}
	}
	for _, c := range with {
		cRepo, cRev, cFile, err := parseHFRef(c.Ref, "")
		if err != nil {
			return nil, err
		}
		if err := add(cRepo, cRev, cFile, "companion", c.Flag); err != nil {
			return nil, err
		}
	}
	if free, err := download.FreeSpace(paths.Home()); err == nil {
		p.FreeDisk = &free
	}
	return p, nil
}
