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
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/ui"
)

var version = "dev"

// Overridable in tests — the real values hit api.github.com.
var (
	releaseAPI  = "https://api.github.com/repos/earshot-run/fornax/releases/latest"
	releaseRepo = "earshot-run/fornax"
	installHint = "go install github.com/earshot-run/fornax@latest"
)

func cmdVersion(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax version")
	}
	fmt.Printf("fornax %s\n", version)
	fmt.Println(ui.Dim("llama.cpp " + catalog.EngineVersion))
	return nil
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	// GitHub reports the uploaded artifact's digest ("sha256:…") — `fornax
	// pins` diffs it against the catalog.
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

func cmdUpgrade(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("upgrade", flag.ExitOnError)
	check := set.Bool("check", false, "report only — do not download or install")
	set.Usage = ui.UsageFunc(set, "usage: fornax upgrade [-check]")
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
			fmt.Println(ui.Dim(rel.HTMLURL))
		}
		return nil
	}
	if *check {
		fmt.Printf("v%s available (you run v%s)\n", latest, current)
		if rel.HTMLURL != "" {
			fmt.Println(ui.Dim(rel.HTMLURL))
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
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		// Anonymous API can't see a private repo — gh carries auth.
		if rel, err := ghLatestRelease(ctx); err == nil {
			return rel, nil
		}
		return &ghRelease{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("api.github.com answered HTTP %d", resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, openai.MaxBody)).Decode(&rel); err != nil {
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

// gh carries auth — private repos' API and asset URLs 404 anonymously.
func ghAvailable() bool {
	if _, err := exec.LookPath("gh"); err != nil {
		return false
	}
	return exec.Command("gh", "auth", "status").Run() == nil
}

func ghLatestRelease(ctx context.Context) (*ghRelease, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", "repos/"+releaseRepo+"/releases/latest").Output()
	if err != nil {
		return nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(out, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// Download one release asset to dir — plain HTTPS first, `gh release
// download` as the authenticated fallback for private repos.
func downloadAsset(ctx context.Context, rel *ghRelease, name, dir string) (string, error) {
	dest := filepath.Join(dir, name)
	if u := findAsset(rel, name); u != "" {
		if body, err := fetchBody(ctx, u); err == nil {
			f, ferr := os.Create(dest)
			if ferr != nil {
				body.Close()
				return "", ferr
			}
			_, copyErr := io.Copy(f, body)
			closeErr := body.Close()
			f.Close()
			if copyErr == nil && closeErr == nil {
				return dest, nil
			}
			os.Remove(dest)
		}
	}
	if ghAvailable() {
		err := exec.CommandContext(ctx, "gh", "release", "download", rel.TagName,
			"--repo", releaseRepo, "-p", name, "--dir", dir, "--clobber").Run()
		if err == nil {
			if info, serr := os.Stat(dest); serr == nil && !info.IsDir() {
				return dest, nil
			}
		}
	}
	return "", fmt.Errorf("could not download %s — %s", name, installHint)
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
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim("downloading fornax "+rel.TagName))
	if err := upgradeBinary(ctx, rel, asset, exe); err != nil {
		return err
	}
	fmt.Printf("%s fornax v%s → v%s\n", ui.Green("✓"), current(), latest)
	return nil
}

// Write the verified asset over exe via a sibling temp file + rename.
func upgradeBinary(ctx context.Context, rel *ghRelease, asset, exe string) error {
	dir, err := os.MkdirTemp("", "fornax-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sumsPath, err := downloadAsset(ctx, rel, "sha256sums.txt", dir)
	if err != nil {
		return err
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			want = fields[0]
		}
	}
	if want == "" {
		return fmt.Errorf("sha256sums.txt in %s has no entry for %s", rel.TagName, asset)
	}
	binPath, err := downloadAsset(ctx, rel, asset, dir)
	if err != nil {
		return err
	}
	payload, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer payload.Close()
	h := sha256.New()
	if _, err := io.Copy(h, payload); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the downloaded binary failed verification (sha256 %s, release says %s)", got, want)
	}
	tmp := exe + ".new"
	in, err := os.Open(binPath)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		in.Close()
		return fmt.Errorf("cannot write next to %s — %w (try sudo, or %s)", exe, err, installHint)
	}
	_, copyErr := io.Copy(out, in)
	in.Close()
	out.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not stage the new binary: %w", copyErr)
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
