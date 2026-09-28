package paths

// Package paths is the filesystem layout under `~/.fornax` (or `$FORNAX_HOME`):
//   engine/llama-cuda/… one unpacked engine build per engine and backend
//   engine/*.part       in-flight engine archives
//   engine/*/installed  the upstream release that build came from
//   models/<id>/<file>  installed weights + projectors
//   models/<id>/*.part  in-flight downloads
//   kev/, laya/         the python runtimes and kev's checkpoints
//   server.key          the loopback API key llama-server enforces
//   server.log          the last server's output, kept for --events
//   config.json         versioned settings
//   custom.json         models added by `pull hf:…` / `pull ollama:…`

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
)

const (
	ConfigFile = "config.json"
	// Builds before 2026-09 wrote the file with a capital C, which macOS's
	// case-insensitive filesystem hid but Linux did not. Read it when the
	// lowercase name is absent so a moved ~/.fornax keeps its key.
	legacyConfigFile = "Config.json"
	// Written last into an engine or runtime dir: what was installed.
	Receipt = "installed"
	KeyFile = "server.key"
)

func Home() string {
	if dir := os.Getenv("FORNAX_HOME"); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".fornax")
	}
	return ".fornax"
}

func ModelDir(root string, spec *catalog.Spec) string {
	return filepath.Join(root, "models", spec.ID)
}

func FilePath(root string, spec *catalog.Spec, file *catalog.Artifact) string {
	return filepath.Join(ModelDir(root, spec), file.File)
}

func PartPath(root string, spec *catalog.Spec, file *catalog.Artifact) string {
	return filepath.Join(ModelDir(root, spec), file.File+".part")
}

func ModelFinal(root string, spec *catalog.Spec) string {
	return FilePath(root, spec, &spec.Model)
}

// Where every engine build installs, one directory each.
func EnginesDir(root string) string {
	return filepath.Join(root, "engine")
}

func EngineDir(root string, spec *catalog.EngineSpec) string {
	return filepath.Join(EnginesDir(root), spec.DirName)
}

// Where an engine archive (the build or one of its parts) downloads to.
func EnginePart(root, archive string) string {
	return filepath.Join(EnginesDir(root), archive+".part")
}

func EngineBinary(root string, spec *catalog.EngineSpec, rel string) string {
	return filepath.Join(EngineDir(root, spec), filepath.FromSlash(rel))
}

func ServerLog(root string) string {
	return filepath.Join(root, "server.log")
}

func KeyPath(root string) string {
	return filepath.Join(root, KeyFile)
}

func EndpointURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

func EngineInstalled(root string, spec *catalog.EngineSpec) bool {
	info, err := os.Stat(EngineBinary(root, spec, spec.Binary))
	if err != nil || info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(EngineDir(root, spec), Receipt))
	return err == nil
}

// The upstream release an installed engine came from, or "".
func EngineRelease(root string, spec *catalog.EngineSpec) string {
	data, err := os.ReadFile(filepath.Join(EngineDir(root, spec), Receipt))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func WriteEngineReceipt(dir, release string) error {
	return AtomicPrivate(filepath.Join(dir, Receipt), []byte(release+"\n"))
}

// How much of a download has landed. A parallel download writes its
// ranges at their offsets into a full-size .part, so its sidecar, not the
// file's size, says how far it got.
func PartialBytes(path string, expected int64) int64 {
	if ranges, ok := ReadPartRanges(path); ok {
		var done int64
		for _, r := range ranges.Ranges {
			done += r.Done
		}
		return min(done, expected)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return min(info.Size(), expected)
}

// A parallel download in progress: each byte range of the file (End
// exclusive) and how much of it has been written.
type PartRanges struct {
	Ranges []PartRange `json:"ranges"`
}

type PartRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

func PartRangesPath(part string) string {
	return part + ".ranges"
}

func ReadPartRanges(part string) (*PartRanges, bool) {
	raw, err := os.ReadFile(PartRangesPath(part))
	if err != nil {
		return nil, false
	}
	var r PartRanges
	if json.Unmarshal(raw, &r) != nil || len(r.Ranges) == 0 {
		return nil, false
	}
	return &r, true
}

func WritePartRanges(part string, r *PartRanges) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return AtomicPrivate(PartRangesPath(part), raw)
}

// Total resumable bytes a model already holds across its .part files.
func ModelPartialBytes(root string, spec *catalog.Spec) int64 {
	var total int64
	for _, file := range spec.Files() {
		total += PartialBytes(PartPath(root, spec, file), file.Bytes)
	}
	return total
}

// Create a directory fornax owns, private to this user on unix.
func ProtectDir(path string) error {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("could not create %s: %w", path, err)
	}
	// MkdirAll does not tighten an existing directory's mode.
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("could not protect %s: %w", path, err)
	}
	return nil
}

// Write bytes atomically (tmp + rename) with owner-only permissions.
func AtomicPrivate(path string, data []byte) error {
	if err := ProtectDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".tmp-write"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("could not write %s: %w", tmp, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("could not save %s: %w", path, err)
	}
	return nil
}

func AtomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return AtomicPrivate(path, data)
}

type Config struct {
	Version int    `json:"version"`
	APIKey  string `json:"apiKey"`
	// A Hugging Face access token, for gated repos. HF_TOKEN wins over it.
	HFToken string `json:"hfToken,omitempty"`
}

// The Hugging Face token to send, or "": HF_TOKEN first, then the one
// saved in config.json.
func HFToken() string {
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token != "" {
		return token
	}
	cfg, err := LoadConfig(Home())
	if err != nil {
		return ""
	}
	return cfg.HFToken
}

// Where the token may go: huggingface.co itself, not its CDNs or anyone else.
func IsHFHost(host string) bool {
	host = strings.ToLower(host)
	return host == "huggingface.co" || host == "hf.co"
}

func LoadConfig(root string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(root, ConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		// A legacy capital-C file, on a case-sensitive filesystem.
		if data, err = os.ReadFile(filepath.Join(root, legacyConfigFile)); errors.Is(err, fs.ErrNotExist) {
			return &Config{Version: 1}, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("could not read fornax settings: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("fornax settings are unreadable; delete %s to reset", filepath.Join(root, ConfigFile))
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("update fornax to read these settings")
	}
	return &cfg, nil
}

func SaveConfig(root string, cfg *Config) error {
	return AtomicJSON(filepath.Join(root, ConfigFile), cfg)
}

// The loopback key llama-server requires and Earshot stores. Created once,
// kept private, reused across runs so a saved Earshot connection survives.
func EnsureKey(root string) (string, error) {
	if err := ProtectDir(root); err != nil {
		return "", err
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		return "", err
	}
	if cfg.APIKey == "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("secure random failed: %w", err)
		}
		cfg.Version = 1
		cfg.APIKey = "esk_local_" + hex.EncodeToString(raw[:])
		if err := SaveConfig(root, cfg); err != nil {
			return "", err
		}
	}
	if err := AtomicPrivate(KeyPath(root), []byte(cfg.APIKey+"\n")); err != nil {
		return "", err
	}
	return cfg.APIKey, nil
}
