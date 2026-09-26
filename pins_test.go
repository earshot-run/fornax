package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
)

func TestParseGHRelease(t *testing.T) {
	owner, repo, tag, asset, ok := parseGHRelease(
		ghHost + "/jaredpalmer/kev/releases/download/kev-family/kev-4b.tar.gz")
	if !ok || owner != "jaredpalmer" || repo != "kev" || tag != "kev-family" || asset != "kev-4b.tar.gz" {
		t.Fatalf("parseGHRelease = %q %q %q %q %v", owner, repo, tag, asset, ok)
	}
	for _, bad := range []string{
		ghHost + "/o/r/releases/tag/x",
		ghHost + "/o/r/archive/deadbeef.tar.gz",
		"https://example.com/o/r/releases/download/t/a",
	} {
		if _, _, _, _, ok := parseGHRelease(bad); ok {
			t.Errorf("parseGHRelease(%q) should fail", bad)
		}
	}
}

func TestParseGHArchive(t *testing.T) {
	sha := "86db6d924cee68fa9a1319d2c3e6010b9b233d60"
	owner, repo, rev, ok := parseGHArchive(ghHost + "/jaredpalmer/kev/archive/" + sha + ".tar.gz")
	if !ok || owner != "jaredpalmer" || repo != "kev" || rev != sha {
		t.Fatalf("parseGHArchive = %q %q %q %v", owner, repo, rev, ok)
	}
}

// A stand-in huggingface.co: HEAD answers the pin headers; GET serves the
// file body for the non-LFS fallback.
func fakeHF(t *testing.T, lfs bool, etag, size, commit string, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("x-repo-commit", commit)
			if lfs {
				w.Header().Set("x-linked-etag", fmt.Sprintf("%q", etag))
				w.Header().Set("x-linked-size", size)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write(body)
	}))
}

func hfSpecPin() (*catalog.Spec, *catalog.Pin) {
	return &catalog.Spec{ID: "test", Repo: "o/r"},
		&catalog.Pin{File: "f.gguf", Revision: "oldcommit", Bytes: 4, SHA256: "deadbeef"}
}

func TestAuditHFFresh(t *testing.T) {
	payload := []byte("weights")
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	srv := fakeHF(t, true, sum, fmt.Sprint(len(payload)), "newcommit", payload)
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	defer func() { modelrt.HFHost = old }()

	spec, pin := hfSpecPin()
	pin.Bytes = int64(len(payload))
	pin.SHA256 = sum
	pin.Revision = "newcommit"
	row := auditHF(context.Background(), spec, pin)
	if row.verdict != pinFresh {
		t.Fatalf("want fresh, got %v (%s)", row.verdict, row.note)
	}
}

// The repo advanced but the file's bytes did not — still fresh.
func TestAuditHFRepoMovedFileSame(t *testing.T) {
	payload := []byte("weights")
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	srv := fakeHF(t, true, sum, fmt.Sprint(len(payload)), "newcommit", payload)
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	defer func() { modelrt.HFHost = old }()

	spec, pin := hfSpecPin()
	pin.Bytes = int64(len(payload))
	pin.SHA256 = sum
	row := auditHF(context.Background(), spec, pin)
	if row.verdict != pinFresh {
		t.Fatalf("want fresh, got %v (%s)", row.verdict, row.note)
	}
}

func TestAuditHFMoved(t *testing.T) {
	payload := []byte("new weights")
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	srv := fakeHF(t, true, sum, fmt.Sprint(len(payload)), "newcommit", payload)
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	defer func() { modelrt.HFHost = old }()

	spec, pin := hfSpecPin()
	row := auditHF(context.Background(), spec, pin)
	if row.verdict != pinMoved || row.pin == nil {
		t.Fatalf("want moved with a re-pin, got %v", row.verdict)
	}
	if row.pin.SHA256 != sum || row.pin.Bytes != int64(len(payload)) || row.pin.Revision != "newcommit" {
		t.Fatalf("bad re-pin: %+v", row.pin)
	}
}

func TestAuditHFGone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	defer func() { modelrt.HFHost = old }()

	spec, pin := hfSpecPin()
	row := auditHF(context.Background(), spec, pin)
	if row.verdict != pinGone {
		t.Fatalf("want gone, got %v (%s)", row.verdict, row.note)
	}
}

// Config files are too small for LFS — no pin headers, so the audit fetches
// and hashes the body.
func TestAuditHFNonLFS(t *testing.T) {
	payload := []byte(`{"arch": "test"}`)
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	srv := fakeHF(t, false, "", "", "commit1", payload)
	defer srv.Close()
	old := modelrt.HFHost
	modelrt.HFHost = srv.URL
	defer func() { modelrt.HFHost = old }()

	spec, pin := hfSpecPin()
	pin.Bytes = int64(len(payload))
	pin.SHA256 = sum
	pin.Revision = "commit1"
	if row := auditHF(context.Background(), spec, pin); row.verdict != pinFresh {
		t.Fatalf("want fresh, got %v (%s)", row.verdict, row.note)
	}
	pin.SHA256 = "deadbeef"
	row := auditHF(context.Background(), spec, pin)
	if row.verdict != pinMoved || row.pin.SHA256 != sum {
		t.Fatalf("want moved with the fetched hash, got %v", row.verdict)
	}
}

func TestAuditReleaseAsset(t *testing.T) {
	payload := []byte("checkpoint tarball")
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/jaredpalmer/kev/releases/tags/kev-family" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"kev-family","assets":[{"name":"k.tar.gz","size":%d,"digest":"sha256:%s"}]}`,
			len(payload), sum)
	}))
	defer api.Close()
	old := ghAPI
	ghAPI = api.URL
	defer func() { ghAPI = old }()

	url := ghHost + "/jaredpalmer/kev/releases/download/kev-family/k.tar.gz"
	row := auditReleaseAsset(context.Background(), url, "kev-family", int64(len(payload)), sum)
	if row.verdict != pinFresh {
		t.Fatalf("want fresh, got %v (%s)", row.verdict, row.note)
	}
	row = auditReleaseAsset(context.Background(), url, "kev-family", int64(len(payload)), "deadbeef")
	if row.verdict != pinMoved || row.pin.SHA256 != sum {
		t.Fatalf("want moved with the asset digest, got %v", row.verdict)
	}
	row = auditReleaseAsset(context.Background(),
		ghHost+"/jaredpalmer/kev/releases/download/kev-family/missing.tar.gz", "kev-family", 1, "x")
	if row.verdict != pinGone {
		t.Fatalf("want gone for a missing asset, got %v", row.verdict)
	}
}

// A moved source archive is small enough to fetch and hash — the re-pin
// comes out complete.
func TestAuditSourceMoved(t *testing.T) {
	archive := []byte("the new source tarball")
	sum := fmt.Sprintf("%x", sha256.Sum256(archive))
	head := "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/jaredpalmer/kev/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"sha":%q}`, head)
	})
	mux.HandleFunc("/jaredpalmer/kev/archive/"+head+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	oldAPI, oldHost := ghAPI, ghHost
	ghAPI, ghHost = srv.URL, srv.URL
	defer func() { ghAPI, ghHost = oldAPI, oldHost }()

	pin := &catalog.Pin{File: "kev-86db6d92.tar.gz", Revision: "86db6d924cee68fa9a1319d2c3e6010b9b233d60", Bytes: 100, SHA256: "old"}
	row := auditSource(context.Background(), "kev", pin, ghHost+"/jaredpalmer/kev/archive/"+pin.Revision+".tar.gz")
	if row.verdict != pinMoved || row.pin == nil {
		t.Fatalf("want moved with a re-pin, got %v", row.verdict)
	}
	if row.pin.Revision != head || row.pin.SHA256 != sum || row.pin.Bytes != int64(len(archive)) {
		t.Fatalf("bad re-pin: %+v", row.pin)
	}
	if row.pin.File != "kev-"+head[:8]+".tar.gz" {
		t.Fatalf("bad re-pin name: %s", row.pin.File)
	}
}

func TestPinJobsFiltersToOneModel(t *testing.T) {
	jobs, err := pinJobs("kev-4b")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].id != "kev-4b" {
		t.Fatalf("want just kev-4b, got %+v", jobs)
	}
	if _, err := pinJobs("nope"); err == nil {
		t.Fatal("want an error for an unknown model")
	}
	if jobs, err := pinJobs(""); err != nil || len(jobs) == 0 {
		t.Fatal("the full audit should produce jobs")
	}
}
