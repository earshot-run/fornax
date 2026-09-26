package paths

// Package paths is the filesystem layout under `~/.fornax` (or `$FORNAX_HOME`):
//   engine/b11060/…     one unpacked llama.cpp release
//   engine/*.part       in-flight engine archives
//   models/<id>/<file>  installed weights + projectors
//   models/<id>/*.part  in-flight downloads
//   …/verified.sha256   the sha256 lines for that engine or model
//   kev/                the kev runtime and its checkpoints
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
	ConfigFile = "Config.json"
	Receipt    = "verified.sha256"
	KeyFile    = "server.key"
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

func FilePath(root string, spec *catalog.Spec, pin *catalog.Pin) string {
	return filepath.Join(ModelDir(root, spec), pin.File)
}

func PartPath(root string, spec *catalog.Spec, pin *catalog.Pin) string {
	return filepath.Join(ModelDir(root, spec), pin.File+".part")
}

func ModelFinal(root string, spec *catalog.Spec) string {
	return FilePath(root, spec, &spec.Model)
}

func EngineDir(root string) string {
	return filepath.Join(root, "engine", catalog.EngineVersion)
}

func EnginePart(root string, spec *catalog.EngineSpec) string {
	return filepath.Join(root, "engine", spec.Archive+".part")
}

func EngineBinary(root string, spec *catalog.EngineSpec, rel string) string {
	return filepath.Join(EngineDir(root), filepath.FromSlash(rel))
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

func WriteReceipt(dir string, spec *catalog.Spec) error {
	var lines strings.Builder
	for _, pin := range spec.Files() {
		fmt.Fprintf(&lines, "%s  %s\n", pin.SHA256, pin.File)
	}
	return AtomicPrivate(filepath.Join(dir, Receipt), []byte(lines.String()))
}

func ReadReceipt(path string, spec *catalog.Spec) bool {
	value, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(value), "\n") {
		if sha, name, ok := strings.Cut(line, "  "); ok {
			got[name] = sha
		}
	}
	for _, pin := range spec.Files() {
		if got[pin.File] != pin.SHA256 {
			return false
		}
	}
	return true
}

func EngineInstalled(root string, spec *catalog.EngineSpec) bool {
	info, err := os.Stat(EngineBinary(root, spec, spec.Binary))
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(EngineDir(root), Receipt))
	return err == nil && strings.TrimSpace(string(data)) == spec.SHA256
}

func WriteEngineReceipt(dir, sha256 string) error {
	return AtomicPrivate(filepath.Join(dir, Receipt), []byte(sha256+"\n"))
}

func PartialBytes(path string, expected int64) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return min(info.Size(), expected)
}

// Total resumable bytes a model already holds across its .part files.
func ModelPartialBytes(root string, spec *catalog.Spec) int64 {
	var total int64
	for _, pin := range spec.Files() {
		total += PartialBytes(PartPath(root, spec, pin), pin.Bytes)
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
}

func LoadConfig(root string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(root, ConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{Version: 1}, nil
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
