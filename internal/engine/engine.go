package engine

// Package engine installs an engine build — llama.cpp or
// stable-diffusion.cpp, any backend — from the newest upstream release that
// carries it: find the archives, download each to `.part`, unpack into a
// staging dir, rename into place, write the receipt naming the release.
// The unpackers are exported because the kev tarball arrives the same way.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/download"
	"github.com/earshot-run/fornax/internal/paths"
)

const (
	// A zip-bomb guard, not a budget: the CUDA runtime alone expands past
	// 1.5 GB.
	maxEngineExpanded = 4 << 30
	maxEngineEntries  = 4_096
)

// Variables so tests can stand in for GitHub, PyPI and the container
// registry.
var (
	githubAPI      = "https://api.github.com"
	pypiAPI        = "https://pypi.org/pypi"
	registryScheme = "https"
)

// One file an install downloads, and for a part, the directory of it that
// lands beside the binary.
type archive struct {
	name  string
	url   string
	bytes int64
	dir   string
}

// progress sees the bytes fetched across every archive of the build, and
// their total.
func Ensure(ctx context.Context, root string, spec *catalog.EngineSpec, progress func(done, total int64)) error {
	if paths.EngineInstalled(root, spec) {
		return nil
	}
	archives, release, err := resolve(ctx, spec)
	if err != nil {
		return err
	}
	var total, done int64
	for _, a := range archives {
		total += a.bytes
	}
	for _, a := range archives {
		offset := done
		if err := download.Fetch(ctx, a.url, a.bytes, paths.EnginePart(root, a.name), func(n int64) { progress(offset+n, total) }); err != nil {
			return err
		}
		done += a.bytes
	}
	if err := install(root, spec, archives, release); err != nil {
		return err
	}
	for _, a := range archives {
		os.Remove(paths.EnginePart(root, a.name))
	}
	return nil
}

// The build's archives, main one first, and the release they come from.
func resolve(ctx context.Context, spec *catalog.EngineSpec) ([]archive, string, error) {
	var main archive
	var release string
	var err error
	if spec.Image != "" {
		main, release, err = resolveImage(ctx, spec)
	} else {
		main, release, err = resolveAsset(ctx, spec.Name, spec.Repo, spec.Asset)
	}
	if err != nil {
		return nil, "", err
	}
	archives := []archive{main}
	for _, part := range spec.Parts {
		var a archive
		if part.PyPI != "" {
			a, err = resolveWheel(ctx, part.PyPI, part.Asset)
		} else {
			a, _, err = resolveAsset(ctx, spec.Name, part.Repo, part.Asset)
		}
		if err != nil {
			return nil, "", err
		}
		a.dir = part.Dir
		archives = append(archives, a)
	}
	return archives, release, nil
}

// The newest release of repo with an asset matching pattern — upstreams
// publish builds as pre-releases, and a release still uploading may lack
// it. Several matches (two CUDA versions) take the highest name.
func resolveAsset(ctx context.Context, name, repo, pattern string) (archive, string, error) {
	match, err := regexp.Compile(pattern)
	if err != nil {
		return archive{}, "", err
	}
	var releases []struct {
		Tag    string `json:"tag_name"`
		Draft  bool   `json:"draft"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, githubAPI+"/repos/"+repo+"/releases?per_page=20", githubHeaders(), &releases); err != nil {
		return archive{}, "", fmt.Errorf("could not list %s releases: %w", name, err)
	}
	for _, r := range releases {
		var found []archive
		for _, a := range r.Assets {
			if !r.Draft && match.MatchString(a.Name) {
				found = append(found, archive{name: a.Name, url: a.URL, bytes: a.Size})
			}
		}
		if len(found) > 0 {
			return slices.MaxFunc(found, func(a, b archive) int { return strings.Compare(a.name, b.name) }), r.Tag, nil
		}
	}
	return archive{}, "", fmt.Errorf("no recent %s release has a build for %s/%s (%s)", name, runtime.GOOS, runtime.GOARCH, pattern)
}

// The newest release of a PyPI package, as the wheel matching pattern.
func resolveWheel(ctx context.Context, pkg, pattern string) (archive, error) {
	match, err := regexp.Compile(pattern)
	if err != nil {
		return archive{}, err
	}
	var project struct {
		URLs []struct {
			Filename string `json:"filename"`
			URL      string `json:"url"`
			Size     int64  `json:"size"`
		} `json:"urls"`
	}
	if err := getJSON(ctx, pypiAPI+"/"+pkg+"/json", nil, &project); err != nil {
		return archive{}, fmt.Errorf("could not look up %s on PyPI: %w", pkg, err)
	}
	for _, u := range project.URLs {
		if match.MatchString(u.Filename) {
			return archive{name: u.Filename, url: u.URL, bytes: u.Size}, nil
		}
	}
	return archive{}, fmt.Errorf("PyPI's %s has no wheel for %s/%s", pkg, runtime.GOOS, runtime.GOARCH)
}

// An API token raises GitHub's anonymous limit of 60 requests an hour.
func githubHeaders() map[string]string {
	headers := map[string]string{"Accept": "application/vnd.github+json"}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// The layer of a public container image that holds the build: the one the
// image's history says a step mentioning spec.ImageLayer created.
func resolveImage(ctx context.Context, spec *catalog.EngineSpec) (archive, string, error) {
	host, rest, ok := strings.Cut(spec.Image, "/")
	repo, tag, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 {
		return archive{}, "", fmt.Errorf("bad image reference %q", spec.Image)
	}
	base := registryScheme + "://" + host + "/v2/" + repo
	var auth struct {
		Token string `json:"token"`
	}
	if err := getJSON(ctx, registryScheme+"://"+host+"/token?scope=repository:"+repo+":pull", nil, &auth); err != nil {
		return archive{}, "", fmt.Errorf("could not reach %s: %w", host, err)
	}
	headers := map[string]string{"Authorization": "Bearer " + auth.Token,
		"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
			"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"}
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
		Platform  struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	}
	var manifest struct {
		Manifests []descriptor `json:"manifests"`
		Config    descriptor   `json:"config"`
		Layers    []descriptor `json:"layers"`
	}
	if err := getJSON(ctx, base+"/manifests/"+tag, headers, &manifest); err != nil {
		return archive{}, "", fmt.Errorf("could not read %s: %w", spec.Image, err)
	}
	if len(manifest.Manifests) > 0 {
		i := slices.IndexFunc(manifest.Manifests, func(d descriptor) bool {
			return d.Platform.OS == runtime.GOOS && d.Platform.Architecture == runtime.GOARCH
		})
		if i < 0 {
			return archive{}, "", fmt.Errorf("%s has no %s/%s image", spec.Image, runtime.GOOS, runtime.GOARCH)
		}
		if err := getJSON(ctx, base+"/manifests/"+manifest.Manifests[i].Digest, headers, &manifest); err != nil {
			return archive{}, "", fmt.Errorf("could not read %s: %w", spec.Image, err)
		}
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
		History []struct {
			CreatedBy  string `json:"created_by"`
			EmptyLayer bool   `json:"empty_layer"`
		} `json:"history"`
	}
	if err := getJSON(ctx, base+"/blobs/"+manifest.Config.Digest, headers, &config); err != nil {
		return archive{}, "", fmt.Errorf("could not read %s: %w", spec.Image, err)
	}
	layer := -1
	for _, h := range config.History {
		if h.EmptyLayer {
			continue
		}
		layer++
		if strings.Contains(h.CreatedBy, spec.ImageLayer) && layer < len(manifest.Layers) {
			found := manifest.Layers[layer]
			if !strings.HasSuffix(found.MediaType, "gzip") {
				return archive{}, "", fmt.Errorf("%s's build layer is %s, not a gzip tarball", spec.Image, found.MediaType)
			}
			url, err := blobLocation(ctx, base+"/blobs/"+found.Digest, auth.Token)
			if err != nil {
				return archive{}, "", err
			}
			release := tag
			if revision := config.Config.Labels["org.opencontainers.image.revision"]; len(revision) >= 7 {
				release += "@" + revision[:7]
			}
			return archive{name: spec.DirName + ".tar.gz", url: url, bytes: found.Size}, release, nil
		}
	}
	return archive{}, "", fmt.Errorf("%s has no layer from a step mentioning %s", spec.Image, spec.ImageLayer)
}

// Registries answer a blob GET with a redirect to short-lived storage that
// needs no token; that URL is what the download fetches.
func blobLocation(ctx context.Context, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach the registry: %w", err)
	}
	resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || location == "" {
		return "", fmt.Errorf("the registry answered HTTP %d instead of a download link", resp.StatusCode)
	}
	resolved, err := resp.Request.URL.Parse(location)
	if err != nil {
		return "", fmt.Errorf("the registry sent a bad download link: %w", err)
	}
	return resolved.String(), nil
}

func getJSON(ctx context.Context, url string, headers map[string]string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fornax")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(into)
}

func install(root string, spec *catalog.EngineSpec, archives []archive, release string) error {
	if err := paths.ProtectDir(root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".engine-")
	if err != nil {
		return fmt.Errorf("could not stage %s: %w", spec.Name, err)
	}
	defer os.RemoveAll(staging)
	if err := unpack(archives[0], root, staging); err != nil {
		return err
	}
	// Releases wrap the build in a directory or two (llama-b11200/,
	// sd.cpp/bin/); the binaries sit in the innermost one.
	top := unwrap(staging)
	for _, part := range archives[1:] {
		if err := unpackBeside(part, root, top); err != nil {
			return err
		}
	}
	if info, statErr := os.Stat(filepath.Join(top, spec.Binary)); statErr != nil || info.IsDir() {
		return fmt.Errorf("the %s %s build has no %s", spec.Name, release, spec.Binary)
	}
	finalDir := paths.EngineDir(root, spec)
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("could not replace managed %s: %w", spec.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return fmt.Errorf("could not install %s: %w", spec.Name, err)
	}
	if err := os.Rename(top, finalDir); err != nil {
		return fmt.Errorf("could not install %s: %w", spec.Name, err)
	}
	return paths.WriteEngineReceipt(finalDir, release)
}

// The innermost directory of a chain of single-directory wrappers.
func unwrap(dir string) string {
	for {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return dir
		}
		dir = filepath.Join(dir, entries[0].Name())
	}
}

func unpack(a archive, root, into string) error {
	part := paths.EnginePart(root, a.name)
	// A wheel is a zip.
	if strings.HasSuffix(a.name, ".zip") || strings.HasSuffix(a.name, ".whl") {
		return UnpackZip(part, into)
	}
	return UnpackTarGz(part, into)
}

// A part's files land next to the binary, which is where the loader looks
// (the exe's own dir on Windows, LD_LIBRARY_PATH on Linux): the part's dir,
// or, without one, the archive less its wrapping directories.
func unpackBeside(a archive, root, binaryDir string) error {
	scratch, err := os.MkdirTemp(root, ".engine-part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := unpack(a, root, scratch); err != nil {
		return err
	}
	from := unwrap(scratch)
	if a.dir != "" {
		if err := safeRelative(a.dir); err != nil {
			return err
		}
		from = filepath.Join(scratch, filepath.FromSlash(a.dir))
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		destination := filepath.Join(binaryDir, entry.Name())
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("%s would overwrite %s from the engine itself", a.name, entry.Name())
		}
		if err := os.Rename(filepath.Join(from, entry.Name()), destination); err != nil {
			return fmt.Errorf("could not unpack %s: %w", a.name, err)
		}
	}
	return nil
}

// An archive entry must stay inside the staging dir: plain relative names only.
func safeRelative(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return fmt.Errorf("the archive contains an unsafe path: %s", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return fmt.Errorf("the archive contains an unsafe path: %s", name)
		}
	}
	return nil
}

func UnpackTarGz(archive, into string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("could not open the archive: %w", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("could not read the archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for index := 0; ; index++ {
		if index >= maxEngineEntries {
			return fmt.Errorf("the archive has too many entries")
		}
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("could not read the archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		// macOS tars bake in AppleDouble sidecars; they are never content.
		if strings.HasPrefix(filepath.Base(header.Name), "._") {
			continue
		}
		if err := safeRelative(header.Name); err != nil {
			return err
		}
		if seen[header.Name] {
			return fmt.Errorf("the archive repeats a path: %s", header.Name)
		}
		seen[header.Name] = true
		destination := filepath.Join(into, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("could not unpack: %w", err)
			}
		case tar.TypeReg:
			total += header.Size
			if total > maxEngineExpanded {
				return fmt.Errorf("the archive expands too large")
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return fmt.Errorf("could not unpack: %w", err)
			}
			mode := os.FileMode(header.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			if err := writeEntry(reader, destination, mode, header.Size); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := safeRelative(header.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return fmt.Errorf("could not unpack: %w", err)
			}
			if err := os.Symlink(header.Linkname, destination); err != nil {
				return fmt.Errorf("could not unpack link: %w", err)
			}
		default:
			return fmt.Errorf("the archive contains an unsupported entry")
		}
	}
}

func UnpackZip(archive, into string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("could not read the archive: %w", err)
	}
	defer reader.Close()
	if len(reader.File) > maxEngineEntries {
		return fmt.Errorf("the archive has too many entries")
	}
	seen := map[string]bool{}
	var total int64
	for _, entry := range reader.File {
		if strings.HasPrefix(filepath.Base(entry.Name), "._") || strings.Contains(entry.Name, "__MACOSX/") {
			continue
		}
		if err := safeRelative(entry.Name); err != nil {
			return err
		}
		if seen[entry.Name] {
			return fmt.Errorf("the archive repeats a path: %s", entry.Name)
		}
		seen[entry.Name] = true
		destination := filepath.Join(into, filepath.FromSlash(entry.Name))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("could not unpack: %w", err)
			}
			continue
		}
		total += int64(entry.UncompressedSize64)
		if total > maxEngineExpanded {
			return fmt.Errorf("the archive expands too large")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("could not unpack: %w", err)
		}
		mode := entry.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		source, err := entry.Open()
		if err != nil {
			return fmt.Errorf("could not unpack: %w", err)
		}
		err = writeEntry(source, destination, mode, int64(entry.UncompressedSize64))
		source.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(source io.Reader, destination string, mode os.FileMode, size int64) error {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("could not unpack: %w", err)
	}
	copied, err := io.Copy(output, source)
	if err != nil {
		output.Close()
		return fmt.Errorf("could not unpack: %w", err)
	}
	if copied != size {
		output.Close()
		return fmt.Errorf("the archive ended early")
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return fmt.Errorf("could not finish unpacking: %w", err)
	}
	return output.Close()
}
