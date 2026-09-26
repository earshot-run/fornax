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
	"sync"
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

func TestUpgradeChannel(t *testing.T) {
	oldVersion, oldAPI, oldMainURL := version, releaseAPI, mainBuildURL
	t.Cleanup(func() { version, releaseAPI, mainBuildURL = oldVersion, oldAPI, oldMainURL })
	for _, tc := range []struct {
		name, installed, available, want string
	}{
		{"main-new", "main-" + strings.Repeat("f", 40), "main-" + strings.Repeat("a", 40), "available"},
		{"main-stale", "main-" + strings.Repeat("b", 40), "main-" + strings.Repeat("a", 40), "not newer"},
		{"main-current", "main-" + strings.Repeat("a", 40), "main-" + strings.Repeat("a", 40), "up to date"},
		{"stable-new", "v1.0.0", "v1.1.0", "available"},
		{"stable-older", "v2.0.0", "v1.1.0", "a different release exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/main-build/version.txt":
					if r.Header.Get("Cache-Control") != "no-cache" || r.URL.Query().Get("check") == "" {
						t.Error("channel lookup can reuse a stale release redirect")
					}
					fmt.Fprintln(w, tc.available)
				case "/compare/" + strings.TrimPrefix(tc.installed, "main-") + "..." + strings.TrimPrefix(tc.available, "main-"):
					comparison := "ahead"
					if tc.name == "main-stale" {
						comparison = "behind"
					}
					fmt.Fprintf(w, `{"status":%q}`, comparison)
				case "/releases/latest", "/releases/tags/" + tc.available:
					fmt.Fprintf(w, `{"tag_name":%q}`, tc.available)
				default:
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			version, releaseAPI, mainBuildURL = tc.installed, srv.URL+"/releases/latest", srv.URL+"/main-build/version.txt"
			out := captureUpgradeCheck(t)
			if !strings.Contains(out, tc.want) || !strings.Contains(out, tc.available) {
				t.Fatalf("got %q, want %q and %q", out, tc.want, tc.available)
			}
			want := "/releases/latest"
			if strings.HasPrefix(tc.installed, "main-") {
				want = "/main-build/version.txt"
				if tc.installed != tc.available {
					want += ",/compare/" + strings.TrimPrefix(tc.installed, "main-") + "..." + strings.TrimPrefix(tc.available, "main-")
				}
				if tc.name != "main-stale" {
					want += ",/releases/tags/" + tc.available
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(requests, ",") != want {
				t.Fatalf("channel requests %v, want %s", requests, want)
			}
		})
	}
}

func captureUpgradeCheck(t *testing.T) string {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stdout := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = stdout }()
	if err := cmdUpgrade(context.Background(), []string{"-check"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLatestMainTagRejectsInvalidPointer(t *testing.T) {
	oldURL := mainBuildURL
	t.Cleanup(func() { mainBuildURL = oldURL })
	for _, value := range []string{"", "v1.0.0", "main-xyz", "main-" + strings.Repeat("a", 41)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, value)
		}))
		mainBuildURL = srv.URL
		_, err := latestMainTag(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "invalid main build") {
			t.Fatalf("pointer %q: got %v", value, err)
		}
	}
}
