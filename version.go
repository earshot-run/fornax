package main

// `fornax version` prints the build stamp; `fornax upgrade` checks the
// latest GitHub release and, on unix, swaps the running binary for the new
// asset after checking it against the release's sha256sums.txt. Release
// builds stamp version via -ldflags "-X main.version=$GITHUB_REF_NAME"
// (see .github/workflows/release.yml).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var version = "dev"

// Overridable in tests — the real values hit api.github.com.
var (
	releaseAPI  = "https://api.github.com/repos/earshot-run/fornax/releases/latest"
	installHint = "go install github.com/earshot-run/fornax@latest"
)

func cmdVersion(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax version")
	}
	fmt.Printf("fornax %s\n", version)
	fmt.Println(dim("llama.cpp " + engineVersion))
	return nil
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func cmdUpgrade(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("upgrade", flag.ExitOnError)
	check := set.Bool("check", false, "report only — do not download or install")
	set.Usage = func() { fmt.Fprintln(os.Stderr, "usage: fornax upgrade [-check]") }
	set.Parse(args)
	if set.NArg() != 0 {
		return fmt.Errorf("usage: fornax upgrade [-check]")
	}
	if version == "dev" {
		fmt.Println("dev build — upgrade tracks tagged releases only")
		return nil
	}
	rel, err := latestRelease(ctx, releaseAPI)
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	current := strings.TrimPrefix(version, "v")
	switch {
	case latest == "":
		fmt.Println("no releases yet")
		return nil
	case latest == current:
		fmt.Printf("fornax is up to date (v%s)\n", current)
		return nil
	case compareVersions(latest, current) <= 0:
		fmt.Printf("a different release exists (v%s, you run v%s)\n", latest, current)
		if rel.HTMLURL != "" {
			fmt.Println(dim(rel.HTMLURL))
		}
		return nil
	}
	if *check {
		fmt.Printf("v%s available (you run v%s)\n", latest, current)
		if rel.HTMLURL != "" {
			fmt.Println(dim(rel.HTMLURL))
		}
		return nil
	}
	return performUpgrade(ctx, rel, latest)
}

func latestRelease(ctx context.Context, api string) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fornax")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach api.github.com: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return &ghRelease{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("api.github.com answered HTTP %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPBody)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("the release reply was not readable: %w", err)
	}
	return &rel, nil
}

// The release asset name for this platform, matching release.yml.
func releaseAssetName() string {
	name := "fornax-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func findAsset(rel *ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// Download the platform asset, verify it against sha256sums.txt, and rename
// it over the running binary. Windows cannot replace a running exe, so it
// prints the asset link instead.
func performUpgrade(ctx context.Context, rel *ghRelease, latest string) error {
	asset := releaseAssetName()
	assetURL := findAsset(rel, asset)
	sumsURL := findAsset(rel, "sha256sums.txt")
	if assetURL == "" || sumsURL == "" {
		fmt.Printf("v%s exists but ships no %s build — %s\n", latest, asset, installHint)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		fmt.Printf("v%s is out — download %s and replace %s\n", latest, assetURL, exe)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s\n", dim("downloading fornax "+rel.TagName))
	if err := upgradeBinary(ctx, rel, assetURL, sumsURL, asset, exe); err != nil {
		return err
	}
	fmt.Printf("%s fornax v%s → v%s\n", green("✓"), current(), latest)
	return nil
}

// Write the verified asset over exe via a sibling temp file + rename.
func upgradeBinary(ctx context.Context, rel *ghRelease, assetURL, sumsURL, asset, exe string) error {
	sums, err := fetchText(ctx, sumsURL)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			want = fields[0]
		}
	}
	if want == "" {
		return fmt.Errorf("sha256sums.txt in %s has no entry for %s", rel.TagName, asset)
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("cannot write next to %s — %w (try sudo, or %s)", exe, err, installHint)
	}
	h := sha256.New()
	dl, err := fetchBody(ctx, assetURL)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	_, copyErr := io.Copy(io.MultiWriter(f, h), dl)
	closeErr := dl.Close()
	f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("the download failed: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("the downloaded binary failed verification (sha256 %s, release says %s)", got, want)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not replace %s — %w (try sudo, or %s)", exe, err, installHint)
	}
	return nil
}

func current() string {
	return strings.TrimPrefix(version, "v")
}

func fetchBody(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fornax")
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

func fetchText(ctx context.Context, url string) (string, error) {
	body, err := fetchBody(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxHTTPBody))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Semver-ish ordering: 1.10.0 > 1.9.0. Non-numeric segments compare as
// strings — good enough to know "newer", never silent about "different".
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if xi, xerr := strconv.Atoi(x); xerr == nil {
			if yi, yerr := strconv.Atoi(y); yerr == nil {
				if xi != yi {
					if xi < yi {
						return -1
					}
					return 1
				}
				continue
			}
		}
		if x != y {
			return strings.Compare(x, y)
		}
	}
	return 0
}
