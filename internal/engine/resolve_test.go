package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

// Stands in for GitHub releases, PyPI and a container registry, serving
// every download from /dl/<name>.
func fakeUpstreams(t *testing.T, blobs map[string][]byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	asset := func(name string) map[string]any {
		return map[string]any{"name": name, "size": len(blobs[name]), "browser_download_url": srv.URL + "/dl/" + name}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			w.Write(blobs[strings.TrimPrefix(r.URL.Path, "/dl/")])
		case r.URL.Path == "/repos/org/engine/releases":
			json.NewEncoder(w).Encode([]map[string]any{
				// Newest first; a release still uploading carries no build yet.
				{"tag_name": "b3", "assets": []any{}},
				{"tag_name": "b2", "assets": []any{asset("eng-b2-bin-linux.tar.gz"), asset("rt-b2-cuda-12.4.tar.gz"), asset("rt-b2-cuda-12.8.tar.gz")}},
				{"tag_name": "b1", "assets": []any{asset("eng-b1-bin-linux.tar.gz")}},
			})
		case r.URL.Path == "/pypi/nccl/json":
			json.NewEncoder(w).Encode(map[string]any{"urls": []any{
				map[string]any{"filename": "nccl-2-py3-none-manylinux_x86_64.whl", "size": len(blobs["nccl-2-py3-none-manylinux_x86_64.whl"]),
					"url": srv.URL + "/dl/nccl-2-py3-none-manylinux_x86_64.whl"},
			}})
		case r.URL.Path == "/token":
			fmt.Fprint(w, `{"token":"anon"}`)
		case strings.HasPrefix(r.URL.Path, "/v2/"):
			if r.Header.Get("Authorization") != "Bearer anon" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch strings.TrimPrefix(r.URL.Path, "/v2/org/img/") {
			case "manifests/cuda":
				fmt.Fprintf(w, `{"manifests":[{"digest":"sha256:m","platform":{"os":%q,"architecture":%q}}]}`, runtime.GOOS, runtime.GOARCH)
			case "manifests/sha256:m":
				fmt.Fprintf(w, `{"config":{"digest":"sha256:c"},"layers":[
					{"digest":"sha256:base","mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":1},
					{"digest":"sha256:bin","mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":%d}]}`, len(blobs["layer"]))
			case "blobs/sha256:c":
				fmt.Fprint(w, `{"config":{"Labels":{"org.opencontainers.image.revision":"2f886889e6e8"}},"history":[
					{"created_by":"ADD rootfs"},{"created_by":"ENV X=1","empty_layer":true},
					{"created_by":"COPY /app/build/bin /app/bin # buildkit"}]}`)
			case "blobs/sha256:bin":
				http.Redirect(w, r, "/dl/layer", http.StatusTemporaryRedirect)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureInstallsTheNewestReleaseWithItsParts(t *testing.T) {
	blobs := map[string][]byte{
		"eng-b2-bin-linux.tar.gz": tarball(t, map[string]string{"eng-b2/server": "new"}),
		"eng-b1-bin-linux.tar.gz": tarball(t, map[string]string{"eng-b1/server": "old"}),
		"rt-b2-cuda-12.4.tar.gz":  tarball(t, map[string]string{"librt.so.12.4": "x"}),
		"rt-b2-cuda-12.8.tar.gz":  tarball(t, map[string]string{"librt.so.12": "rt"}),
	}
	srv := fakeUpstreams(t, blobs)
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })
	root := t.TempDir()
	spec := &catalog.EngineSpec{Name: "eng", Repo: "org/engine", Asset: `^eng-[^-]+-bin-linux\.tar\.gz$`, DirName: "eng-cuda",
		Parts: []catalog.EnginePart{{Repo: "org/engine", Asset: `^rt-[^-]+-cuda-12\.\d+\.tar\.gz$`}}, Binary: "server"}
	var last, total int64
	if err := Ensure(t.Context(), root, spec, func(done, all int64) { last, total = done, all }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(paths.EngineBinary(root, spec, "server")); string(got) != "new" {
		t.Fatalf("installed server = %q, want b2's", got)
	}
	if got, _ := os.ReadFile(paths.EngineBinary(root, spec, "librt.so.12")); string(got) != "rt" {
		t.Fatalf("runtime beside the binary = %q, want the highest CUDA 12", got)
	}
	if release := paths.EngineRelease(root, spec); release != "b2" || !paths.EngineInstalled(root, spec) {
		t.Fatalf("release %q, installed %v", release, paths.EngineInstalled(root, spec))
	}
	if last != total || total != int64(len(blobs["eng-b2-bin-linux.tar.gz"])+len(blobs["rt-b2-cuda-12.8.tar.gz"])) {
		t.Fatalf("progress ended at %d of %d", last, total)
	}
}

// A build that ships only inside a container image: the layer the history
// names, plus a wheel of which only one directory lands beside the binary.
func TestEnsureInstallsAnImageLayerAndAWheelDir(t *testing.T) {
	blobs := map[string][]byte{
		"layer": tarball(t, map[string]string{"app/bin/cli": "cli", "app/bin/libx.so": "x"}),
		"nccl-2-py3-none-manylinux_x86_64.whl": zipball(t, map[string]string{
			"nvidia/nccl/lib/libnccl.so.2": "nccl", "nvidia/nccl/include/nccl.h": "h", "nccl-2.dist-info/RECORD": "r"}),
	}
	srv := fakeUpstreams(t, blobs)
	pypiAPI, registryScheme = srv.URL+"/pypi", "http"
	t.Cleanup(func() { pypiAPI, registryScheme = "https://pypi.org/pypi", "https" })
	root := t.TempDir()
	spec := &catalog.EngineSpec{Name: "sd", Image: strings.TrimPrefix(srv.URL, "http://") + "/org/img:cuda", ImageLayer: "/app/bin",
		DirName: "sd-cuda", Binary: "cli",
		Parts: []catalog.EnginePart{{PyPI: "nccl", Asset: `manylinux.*_x86_64\.whl$`, Dir: "nvidia/nccl/lib"}}}
	if err := Ensure(t.Context(), root, spec, func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	dir := paths.EngineDir(root, spec)
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "cli installed libnccl.so.2 libx.so" {
		t.Fatalf("engine dir holds %v", names)
	}
	if release := paths.EngineRelease(root, spec); release != "cuda@2f88688" {
		t.Fatalf("release = %q", release)
	}
}
