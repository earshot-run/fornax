package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestInstallReleaseChecksum(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the shell installer supports macOS and Linux")
	}
	for _, authenticated := range []bool{false, true} {
		for _, scenario := range []string{"main", "tagged", "tampered", "invalid-pointer"} {
			t.Run(fmt.Sprintf("gh=%t/%s", authenticated, scenario), func(t *testing.T) {
				t.Setenv("FORNAX_HOME", t.TempDir())
				root := t.TempDir()
				asset := fmt.Sprintf("fornax-%s-%s", runtime.GOOS, runtime.GOARCH)
				payload := []byte("fake release binary")
				digest := sha256.Sum256(payload)
				tag := "main-" + strings.Repeat("a", 40)
				override := ""
				if scenario == "tagged" {
					tag, override = "v1.0.0", "v1.0.0"
				}
				var mu sync.Mutex
				var requests []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					requests = append(requests, r.URL.Path)
					mu.Unlock()
					switch r.URL.Path {
					case "/main-build/version.txt":
						if !authenticated && r.Header.Get("Cache-Control") != "no-cache" {
							t.Error("channel lookup can reuse a stale release redirect")
						}
						if scenario == "invalid-pointer" {
							fmt.Fprintln(w, "v1.0.0")
						} else {
							fmt.Fprintln(w, tag)
						}
					case "/" + tag + "/" + asset:
						if scenario == "tampered" {
							w.Write([]byte("corrupted binary"))
						} else {
							w.Write(payload)
						}
					case "/" + tag + "/sha256sums.txt":
						fmt.Fprintf(w, "%x  %s\n", digest, asset)
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				bin := filepath.Join(root, "bin")
				if err := os.Mkdir(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				gh := "#!/bin/sh\nexit 1\n"
				if authenticated {
					gh = `#!/bin/sh
set -eu
if [ "$*" = 'auth status' ]; then exit 0; fi
[ "$1 $2" = 'release download' ] || exit 1
tag=$3
shift 3
if [ "$tag" = main-build ]; then
  [ "$*" = '--repo earshot-run/fornax -p version.txt -O -' ] || exit 1
  exec curl -fsSL "$FORNAX_RELEASES/$tag/version.txt"
fi
[ "$1 $2 $3" = '--repo earshot-run/fornax -p' ] || exit 1
asset=$4
shift 4
[ "$1 $2 $3" = '-p sha256sums.txt --dir' ] || exit 1
dir=$4
curl -fsSL "$FORNAX_RELEASES/$tag/$asset" -o "$dir/$asset"
curl -fsSL "$FORNAX_RELEASES/$tag/sha256sums.txt" -o "$dir/sha256sums.txt"
`
				}
				if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o700); err != nil {
					t.Fatal(err)
				}
				dest := filepath.Join(root, "installed")
				if err := os.Mkdir(dest, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, "fornax"), []byte("old"), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("sh", "install.sh")
				cmd.Env = append(os.Environ(),
					"HOME="+root, "FORNAX_INSTALL="+dest, "FORNAX_TAG="+override,
					"FORNAX_RELEASES="+srv.URL, "FORNAX_BASE=", "PATH="+bin+":/usr/bin:/bin")
				out, err := cmd.CombinedOutput()
				if scenario == "tampered" || scenario == "invalid-pointer" {
					if err == nil {
						t.Fatal("accepted an invalid download")
					}
					payload = []byte("old")
				} else if err != nil {
					t.Fatalf("installer failed: %v: %s", err, out)
				} else if !strings.Contains(string(out), "installed ") || !strings.Contains(string(out), tag) {
					t.Fatalf("missing versioned install confirmation: %s", out)
				}
				installed, err := os.ReadFile(filepath.Join(dest, "fornax"))
				if err != nil || !bytes.Equal(installed, payload) {
					t.Fatalf("installed binary %q, want %q: %v", installed, payload, err)
				}
				wantRequests := 3
				if scenario == "invalid-pointer" {
					wantRequests = 1
				}
				if scenario == "tagged" {
					wantRequests = 2
				}
				mu.Lock()
				defer mu.Unlock()
				if len(requests) != wantRequests {
					t.Fatalf("want one channel lookup (none for a tag) and two downloads: %v", requests)
				}
			})
		}
	}
}
