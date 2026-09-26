package main

// `fornax pins` — audit every pinned artifact against upstream: built-in HF
// weights on `main`, kev checkpoints under a moving GitHub release tag, the
// kev/laya runtimes on commit archives, and every saved custom model — all
// can drift from the pin. Moved built-ins print the fresh
// Revision/Bytes/SHA256 to paste into the source; a moved custom means the
// upstream file changed since it was pinned — `rm` and re-pull to re-pin.
// `fornax pins <model>` audits one entry. Exits non-zero when anything moved,
// vanished, or could not be checked, so CI can gate on it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Overridable in tests — the real values hit the public hosts. Hugging
// Face's is modelrt.HFHost, which pinning a ref resolves against too.
var (
	ghHost = "https://github.com"
	ghAPI  = "https://api.github.com"
)

// Small files (configs, tokenizers) are not LFS — no sha256 in the HEAD
// headers — so the audit fetches and hashes them, up to a cap.
const pinProbeCap = 64 << 20

type pinVerdict int

const (
	pinFresh pinVerdict = iota
	pinMoved
	pinGone
	pinError
)

func (v pinVerdict) cell() string {
	switch v {
	case pinMoved:
		return ui.Cell("moved", 7, ui.Yellow)
	case pinGone:
		return ui.Cell("gone", 7, ui.Red)
	case pinError:
		return ui.Cell("error", 7, ui.Red)
	}
	return ui.Cell("fresh", 7, ui.Green)
}

// One row of the audit. pin carries the re-pin values when moved.
type pinRow struct {
	id      string
	file    string
	verdict pinVerdict
	note    string
	pin     *catalog.Pin
}

type pinJob struct {
	id, file string
	check    func(context.Context) pinRow
}

func cmdPins(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("pins", flag.ExitOnError)
	set.Usage = ui.UsageFunc(set, "usage: fornax pins [model]")
	ids := parseFlexible(set, args, -1)
	if len(ids) > 1 {
		return fmt.Errorf("usage: fornax pins [model]")
	}
	only := ""
	if len(ids) == 1 {
		only = ids[0]
	}
	jobs, err := pinJobs(only)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Printf("%s ships in the OS — nothing pinned to check\n", only)
		return nil
	}
	spin := ui.Spin(fmt.Sprintf("auditing %d pins", len(jobs)))
	rows := runPinJobs(ctx, jobs)
	spin.Stop("")

	idW, fileW := 0, 0
	for i := range rows {
		if len(rows[i].id) > idW {
			idW = len(rows[i].id)
		}
		if len(rows[i].file) > fileW {
			fileW = len(rows[i].file)
		}
	}
	var moved, gone, failed int
	for _, row := range rows {
		fmt.Printf("  %s %s %s %s\n",
			ui.Cell(row.id, idW, nil), ui.Cell(row.file, fileW, nil),
			row.verdict.cell(), ui.Dim(row.note))
		switch row.verdict {
		case pinMoved:
			moved++
		case pinGone:
			gone++
		case pinError:
			failed++
		}
	}
	if moved > 0 {
		fmt.Printf("\n%s\n", ui.Bold("re-pin:"))
		for _, row := range rows {
			if row.verdict != pinMoved || row.pin == nil {
				continue
			}
			fmt.Printf("  %s  %s\n", row.id, ui.Dim(row.file))
			if row.pin.Revision != "" {
				fmt.Printf("    Revision: %q, Bytes: %d, SHA256: %q\n",
					row.pin.Revision, row.pin.Bytes, row.pin.SHA256)
			} else {
				fmt.Printf("    Bytes: %d, SHA256: %q\n", row.pin.Bytes, row.pin.SHA256)
			}
		}
	}
	if only == "" {
		noteLatestEngine(ctx, "ggml-org/llama.cpp", catalog.EngineVersion)
		if sd := modelrt.SDEngines(); len(sd) > 0 {
			for _, variants := range sd {
				if _, _, tag, _, ok := parseGHRelease(variants[0].URL); ok {
					noteLatestEngine(ctx, "leejet/stable-diffusion.cpp", tag)
				}
				break
			}
		}
	}
	var drift []string
	if moved > 0 {
		drift = append(drift, fmt.Sprintf("%d moved", moved))
	}
	if gone > 0 {
		drift = append(drift, fmt.Sprintf("%d gone", gone))
	}
	if failed > 0 {
		drift = append(drift, fmt.Sprintf("%d unreachable", failed))
	}
	if len(drift) > 0 {
		return fmt.Errorf("%s", strings.Join(drift, ", "))
	}
	fmt.Printf("%s\n", ui.Dim(fmt.Sprintf("all %d pins fresh", len(rows))))
	return nil
}

// Every pinned artifact, in listing order: model files, then both engines'
// per-platform archives, then the kev/laya source tarballs.
func pinJobs(only string) ([]pinJob, error) {
	var jobs []pinJob
	addSpec := func(spec *catalog.Spec) {
		for _, pin := range spec.Files() {
			job := pinJob{id: spec.ID, file: pin.File}
			switch {
			case pin.URL == "":
				job.check = func(ctx context.Context) pinRow {
					return auditHF(ctx, spec, pin)
				}
			case strings.Contains(pin.URL, "/releases/download/"):
				job.check = func(ctx context.Context) pinRow {
					return auditReleaseAsset(ctx, pin.URL, pin.Revision, pin.Bytes, pin.SHA256)
				}
			case strings.Contains(pin.URL, "huggingface.co/"):
				job.check = func(ctx context.Context) pinRow {
					return auditHFURL(ctx, pin)
				}
			default:
				job.check = func(context.Context) pinRow {
					return pinRow{verdict: pinError, note: "unrecognized upstream " + pin.URL}
				}
			}
			jobs = append(jobs, job)
		}
	}
	if only != "" {
		spec := modelrt.Model(only)
		if spec == nil {
			return nil, fmt.Errorf("unknown model %q — `fornax list` shows what is saved", only)
		}
		addSpec(spec)
		return jobs, nil
	}
	for _, spec := range modelrt.AllSpecs(paths.Home()) {
		addSpec(spec)
	}
	// Every build on every platform, and every part of each build.
	addEngine := func(engines map[string][]*catalog.EngineSpec) {
		platforms := make([]string, 0, len(engines))
		for p := range engines {
			platforms = append(platforms, p)
		}
		sort.Strings(platforms)
		for _, p := range platforms {
			for _, eng := range engines[p] {
				_, repo, tag, _, _ := parseGHRelease(eng.URL)
				id := repo + " " + tag
				note := p + " " + string(eng.Backend)
				archives := append([]catalog.EnginePart{{URL: eng.URL, Bytes: eng.Bytes, SHA256: eng.SHA256, Archive: eng.Archive}}, eng.Parts...)
				for _, archive := range archives {
					jobs = append(jobs, pinJob{id: id, file: archive.Archive,
						check: func(ctx context.Context) pinRow {
							row := auditReleaseAsset(ctx, archive.URL, "", archive.Bytes, archive.SHA256)
							if row.verdict == pinFresh {
								row.note = note
							}
							return row
						}})
				}
			}
		}
	}
	addEngine(catalog.Engines())
	addEngine(modelrt.SDEngines())
	for _, src := range modelrt.Sources() {
		jobs = append(jobs, pinJob{id: src.Name + " source", file: src.Pin.File,
			check: func(ctx context.Context) pinRow {
				return auditSource(ctx, src.Name, &src.Pin, src.URL)
			}})
	}
	return jobs, nil
}

func runPinJobs(ctx context.Context, jobs []pinJob) []pinRow {
	rows := make([]pinRow, len(jobs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				rows[i] = pinRow{id: job.id, file: job.file, verdict: pinError, note: "canceled"}
				return
			}
			row := job.check(ctx)
			row.id, row.file = job.id, job.file
			rows[i] = row
		}()
	}
	wg.Wait()
	return rows
}

// HEAD the file at `main` — the LFS sha256 in x-linked-etag, bytes in
// x-linked-size, the commit it resolves to in x-repo-commit. Non-LFS files
// carry none of those and get fetched and hashed instead.
func auditHF(ctx context.Context, spec *catalog.Spec, pin *catalog.Pin) pinRow {
	url := fmt.Sprintf("%s/%s/resolve/main/%s", modelrt.HFHost, spec.Repo, pin.File)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return pinRow{verdict: pinError, note: err.Error()}
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return pinRow{verdict: pinError, note: "unreachable"}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return pinRow{verdict: pinGone, note: "gated or private"}
	case http.StatusNotFound, http.StatusGone:
		return pinRow{verdict: pinGone, note: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		return pinRow{verdict: pinError, note: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	commit := resp.Header.Get("x-repo-commit")
	sha := strings.Trim(resp.Header.Get("x-linked-etag"), `"`)
	size, _ := strconv.ParseInt(resp.Header.Get("x-linked-size"), 10, 64)
	if sha == "" || size == 0 {
		sha, size, err = fetchSHA(ctx, url)
		if err != nil {
			return pinRow{verdict: pinError, note: err.Error()}
		}
	}
	return comparePin(pin, commit, sha, size, "main")
}

// A companion pinned as a full resolve URL — the revision lives in the URL
// itself, so only digest and size can drift underneath it.
func auditHFURL(ctx context.Context, pin *catalog.Pin) pinRow {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, pin.URL, nil)
	if err != nil {
		return pinRow{verdict: pinError, note: err.Error()}
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return pinRow{verdict: pinError, note: "unreachable"}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return pinRow{verdict: pinGone, note: "gated or private"}
	case http.StatusNotFound, http.StatusGone:
		return pinRow{verdict: pinGone, note: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		return pinRow{verdict: pinError, note: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	sha := strings.Trim(resp.Header.Get("x-linked-etag"), `"`)
	size, _ := strconv.ParseInt(resp.Header.Get("x-linked-size"), 10, 64)
	if sha == "" || size == 0 {
		if sha, size, err = fetchSHA(ctx, pin.URL); err != nil {
			return pinRow{verdict: pinError, note: err.Error()}
		}
	}
	return comparePin(pin, "", sha, size, "upstream")
}

// A GitHub release asset: the releases/tags API reports each asset's size
// and sha256 digest — a re-uploaded artifact under the same tag (kev-family)
// shows up as a changed digest.
func auditReleaseAsset(ctx context.Context, url, rev string, wantBytes int64, wantSHA string) pinRow {
	owner, repo, tag, name, ok := parseGHRelease(url)
	if !ok {
		return pinRow{verdict: pinError, note: "unrecognized release URL"}
	}
	var rel ghRelease
	code, err := ghGet(ctx, "/repos/"+owner+"/"+repo+"/releases/tags/"+tag, &rel)
	if err != nil {
		return pinRow{verdict: pinError, note: err.Error()}
	}
	if code == http.StatusNotFound {
		return pinRow{verdict: pinGone, note: "no release " + tag}
	}
	if code != http.StatusOK {
		return pinRow{verdict: pinError, note: fmt.Sprintf("github HTTP %d", code)}
	}
	var asset *ghAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			asset = &rel.Assets[i]
		}
	}
	if asset == nil {
		return pinRow{verdict: pinGone, note: "no asset " + name}
	}
	sha := strings.TrimPrefix(asset.Digest, "sha256:")
	if sha == "" {
		if asset.Size == wantBytes {
			return pinRow{verdict: pinFresh, note: "@" + tag + " (size only — no upstream digest)"}
		}
		return pinRow{verdict: pinMoved, note: "size changed; no upstream digest to re-pin from"}
	}
	return comparePin(&catalog.Pin{File: name, Revision: rev, Bytes: wantBytes, SHA256: wantSHA},
		rev, sha, asset.Size, tag)
}

// A pinned commit archive (kev/laya runtimes): the repo's HEAD commit vs the
// pinned sha. On drift the new archive is small enough to fetch and hash —
// the re-pin comes out complete.
func auditSource(ctx context.Context, name string, pin *catalog.Pin, url string) pinRow {
	owner, repo, _, ok := parseGHArchive(url)
	if !ok {
		return pinRow{verdict: pinError, note: "unrecognized archive URL"}
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	code, err := ghGet(ctx, "/repos/"+owner+"/"+repo+"/commits/HEAD", &commit)
	if err != nil {
		return pinRow{verdict: pinError, note: err.Error()}
	}
	if code == http.StatusNotFound {
		return pinRow{verdict: pinGone, note: "repo gone"}
	}
	if code != http.StatusOK {
		return pinRow{verdict: pinError, note: fmt.Sprintf("github HTTP %d", code)}
	}
	if commit.SHA == "" || commit.SHA == pin.Revision {
		return pinRow{verdict: pinFresh, note: "@" + shortSHA(pin.Revision)}
	}
	sha, size, err := fetchSHA(ctx, fmt.Sprintf("%s/%s/%s/archive/%s.tar.gz", ghHost, owner, repo, commit.SHA))
	if err != nil {
		return pinRow{verdict: pinMoved, note: "HEAD @" + shortSHA(commit.SHA) + " (re-pin needs a manual hash)"}
	}
	return pinRow{verdict: pinMoved, note: "HEAD @" + shortSHA(commit.SHA),
		pin: &catalog.Pin{
			File:     fmt.Sprintf("%s-%s.tar.gz", name, commit.SHA[:8]),
			Revision: commit.SHA,
			Bytes:    size,
			SHA256:   sha,
		}}
}

// Shared fresh/moved decision: same digest and size means the pin still
// names the same bytes, even if the ref moved on.
func comparePin(pin *catalog.Pin, rev, sha string, size int64, refName string) pinRow {
	at := ""
	if rev != "" {
		at = "@" + shortSHA(rev)
	}
	if sha == pin.SHA256 && size == pin.Bytes {
		if rev != "" && rev != pin.Revision {
			return pinRow{verdict: pinFresh, note: "bytes unchanged — " + refName + " now " + at}
		}
		return pinRow{verdict: pinFresh, note: at}
	}
	note := refName + " moved"
	if at != "" {
		note += " → " + at
	}
	return pinRow{verdict: pinMoved, note: note,
		pin: &catalog.Pin{File: pin.File, Revision: rev, Bytes: size, SHA256: sha}}
}

// GET and hash a whole artifact — the fallback for non-LFS HF files and for
// moved source archives. Capped: this is a probe, not a pull.
func fetchSHA(ctx context.Context, url string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return "", 0, fmt.Errorf("HTTP %d fetching the artifact", resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, pinProbeCap+1))
	if err != nil {
		return "", 0, err
	}
	if n > pinProbeCap {
		return "", 0, fmt.Errorf("no LFS headers and too large to hash")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func ghGet(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ghAPI+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fornax")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach api.github.com")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, openai.MaxBody)).Decode(out); err != nil {
		return 0, fmt.Errorf("the github reply was not readable")
	}
	return http.StatusOK, nil
}

// github.com/<owner>/<repo>/releases/download/<tag>/<asset>
func parseGHRelease(url string) (owner, repo, tag, asset string, ok bool) {
	rest, found := strings.CutPrefix(url, ghHost+"/")
	if !found {
		return "", "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 6 || parts[2] != "releases" || parts[3] != "download" {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[4], parts[5], true
}

// github.com/<owner>/<repo>/archive/<sha>.tar.gz
func parseGHArchive(url string) (owner, repo, rev string, ok bool) {
	rest, found := strings.CutPrefix(url, ghHost+"/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[2] != "archive" {
		return "", "", "", false
	}
	rev, isTar := strings.CutSuffix(parts[3], ".tar.gz")
	if !isTar || rev == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], rev, true
}

// Commits shorten to 8; tag names print whole.
func shortSHA(sha string) string {
	if len(sha) == 40 {
		return sha[:8]
	}
	return sha
}

// Engines pin a chosen release, so a newer upstream tag is information, not
// drift — a dim footer line, never a failure.
func noteLatestEngine(ctx context.Context, repo, pinned string) {
	var rel ghRelease
	code, err := ghGet(ctx, "/repos/"+repo+"/releases/latest", &rel)
	if err != nil || code != http.StatusOK || rel.TagName == "" || rel.TagName == pinned {
		return
	}
	fmt.Printf("%s\n", ui.Dim(fmt.Sprintf("note: %s latest release is %s — pinned %s", repo, rel.TagName, pinned)))
}
