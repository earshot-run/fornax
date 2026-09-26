package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallPublicReleaseChecksum(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the shell installer supports macOS and Linux")
	}
	t.Setenv("FORNAX_HOME", t.TempDir())
	root := t.TempDir()
	asset := fmt.Sprintf("fornax-%s-%s", runtime.GOOS, runtime.GOARCH)
	payload := []byte("fake release binary")
	digest := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			w.Write(payload)
		case "/sha256sums.txt":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Force the public curl path even on development machines with gh auth.
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "installed")
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(),
		"HOME="+root, "FORNAX_INSTALL="+dest, "FORNAX_TAG=v1.0.0", "FORNAX_BASE="+srv.URL,
		"PATH="+bin+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer failed: %v: %s", err, out)
	}
	installed, err := os.ReadFile(filepath.Join(dest, "fornax"))
	if err != nil || !bytes.Equal(installed, payload) {
		t.Fatalf("installed binary differs from checksum-verified release: %v", err)
	}
	if !strings.Contains(string(out), "installed ") {
		t.Fatalf("missing install confirmation: %s", out)
	}
}
