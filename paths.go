package main

// Filesystem layout under `~/.fornax` (or `$FORNAX_HOME`):
//   engine/b11060/…     one unpacked llama.cpp release
//   engine/*.part       in-flight engine archives
//   models/<id>/<file>  installed weights + projectors
//   models/<id>/*.part  in-flight downloads
//   verified.sha256     sha256sum lines, one per installed file
//   server.key          the loopback API key llama-server enforces
//   config.json         versioned settings

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
)

const (
	configFile = "config.json"
	receipt    = "verified.sha256"
	keyFile    = "server.key"
)

func home() string {
	if dir := os.Getenv("FORNAX_HOME"); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".fornax")
	}
	return ".fornax"
}

func modelDir(root string, spec *modelSpec) string {
	return filepath.Join(root, "models", spec.id)
}

func filePath(root string, spec *modelSpec, pin *filePin) string {
	return filepath.Join(modelDir(root, spec), pin.file)
}

func partPath(root string, spec *modelSpec, pin *filePin) string {
	return filepath.Join(modelDir(root, spec), pin.file+".part")
}

func modelFinal(root string, spec *modelSpec) string {
	return filePath(root, spec, &spec.model)
}

func engineDir(root string) string {
	return filepath.Join(root, "engine", engineVersion)
}

func enginePart(root string, spec *engineSpec) string {
	return filepath.Join(root, "engine", spec.archive+".part")
}

func engineBinary(root string, spec *engineSpec, rel string) string {
	return filepath.Join(engineDir(root), filepath.FromSlash(rel))
}

func serverLog(root string) string {
	return filepath.Join(root, "server.log")
}

func keyPath(root string) string {
	return filepath.Join(root, keyFile)
}

func endpointURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", port)
}

// A model is installed when every pinned file is present at its byte count
// and the receipt lists the matching digests. kev models keep their verified
// tarball and the checkpoint it unpacks to.
func modelInstalled(root string, spec *modelSpec) bool {
	if spec.rt == runtimeApple {
		return appleInstalled(root, spec)
	}
	dir := modelDir(root, spec)
	for _, pin := range spec.files() {
		info, err := os.Stat(filePath(root, spec, pin))
		if err != nil || info.Size() != pin.bytes {
			return false
		}
	}
	if spec.rt == runtimeKev && kevCkptDir(root, spec) == "" {
		return false
	}
	return readReceipt(filepath.Join(dir, receipt), spec)
}

func writeReceipt(dir string, spec *modelSpec) error {
	var lines strings.Builder
	for _, pin := range spec.files() {
		fmt.Fprintf(&lines, "%s  %s\n", pin.sha256, pin.file)
	}
	return atomicPrivate(filepath.Join(dir, receipt), []byte(lines.String()))
}

func readReceipt(path string, spec *modelSpec) bool {
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
	for _, pin := range spec.files() {
		if got[pin.file] != pin.sha256 {
			return false
		}
	}
	return true
}

func engineInstalled(root string, spec *engineSpec) bool {
	info, err := os.Stat(engineBinary(root, spec, spec.binary))
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(engineDir(root), receipt))
	return err == nil && strings.TrimSpace(string(data)) == spec.sha256
}

func writeEngineReceipt(dir, sha256 string) error {
	return atomicPrivate(filepath.Join(dir, receipt), []byte(sha256+"\n"))
}

func partialBytes(path string, expected int64) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return min(info.Size(), expected)
}

// Total resumable bytes a model already holds across its .part files.
func modelPartialBytes(root string, spec *modelSpec) int64 {
	var total int64
	for _, pin := range spec.files() {
		total += partialBytes(partPath(root, spec, pin), pin.bytes)
	}
	return total
}

// Create a directory fornax owns, private to this user on unix.
func protectDir(path string) error {
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
func atomicPrivate(path string, data []byte) error {
	if err := protectDir(filepath.Dir(path)); err != nil {
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

func atomicJSONPrivate(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivate(path, data)
}

type config struct {
	Version int    `json:"version"`
	APIKey  string `json:"apiKey"`
}

func loadConfig(root string) (*config, error) {
	data, err := os.ReadFile(filepath.Join(root, configFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &config{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read fornax settings: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("fornax settings are unreadable; delete %s to reset", filepath.Join(root, configFile))
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("update fornax to read these settings")
	}
	return &cfg, nil
}

func saveConfig(root string, cfg *config) error {
	return atomicJSONPrivate(filepath.Join(root, configFile), cfg)
}

// The loopback key llama-server requires and Earshot stores. Created once,
// kept private, reused across runs so a saved Earshot connection survives.
func ensureKey(root string) (string, error) {
	if err := protectDir(root); err != nil {
		return "", err
	}
	cfg, err := loadConfig(root)
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
		if err := saveConfig(root, cfg); err != nil {
			return "", err
		}
	}
	if err := atomicPrivate(keyPath(root), []byte(cfg.APIKey+"\n")); err != nil {
		return "", err
	}
	return cfg.APIKey, nil
}
