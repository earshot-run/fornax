package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.10.0", "1.9.0", 1},
		{"0.9.9", "1.0.0", -1},
		{"2.0", "2.0.1", -1},
		{"v1rc1", "v1rc2", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// A release server that serves one asset plus sha256sums.txt; the payload
// can be corrupted per-case.
func fakeRelease(t *testing.T, asset string, payload []byte, corrupt bool) (*ghRelease, *httptest.Server) {
	t.Helper()
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			if corrupt {
				w.Write(append(payload, 'x'))
			} else {
				w.Write(payload)
			}
		case "/sha256sums.txt":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		default:
			http.NotFound(w, r)
		}
	}))
	rel := &ghRelease{TagName: "v9.9.9"}
	rel.Assets = []ghAsset{
		{Name: asset, URL: srv.URL + "/" + asset},
		{Name: "sha256sums.txt", URL: srv.URL + "/sha256sums.txt"},
	}
	return rel, srv
}

// upgradeBinary writes the verified asset over the target via rename — the
// same steps performUpgrade runs on os.Executable.
func TestUpgradeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot replace a running exe on windows")
	}
	asset := "fornax-" + runtime.GOOS + "-" + runtime.GOARCH
	payload := []byte("the new fornax binary")
	rel, srv := fakeRelease(t, asset, payload, false)
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "fornax")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := upgradeBinary(context.Background(), rel, asset, exe); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(payload) {
		t.Fatalf("exe holds %q, want the new payload", got)
	}
}

// A payload that fails the checksum must not touch the installed binary.
func TestUpgradeBinaryRejectsTampered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot replace a running exe on windows")
	}
	asset := "fornax-" + runtime.GOOS + "-" + runtime.GOARCH
	rel, srv := fakeRelease(t, asset, []byte("new binary"), true)
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "fornax")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := upgradeBinary(context.Background(), rel, asset, exe)
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("want a verification error, got %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old" {
		t.Fatal("the old binary was overwritten by an unverified download")
	}
}
