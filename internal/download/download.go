package download

// Package download makes resumable downloads. A `.part` file resumes via HTTP
// Range; nothing is "done" until it holds the size the server announced.
//
// Big files come down as parallel byte ranges: one stream is capped by its
// own latency and the server's per-connection pace long before the line is
// full. Each range lands at its offset in a full-size .part, with a
// .part.ranges sidecar recording how far each got, so an interrupted
// download resumes range by range.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/paths"
)

// Below parallelMin a single stream is already quick; above it, ranges pay
// off. Variables so tests can use small files.
var (
	parallelMin int64 = 64 << 20
	maxRanges   int64 = 8
	minRange    int64 = 32 << 20
)

var errNoRanges = errors.New("the server does not serve byte ranges")

// expected is the size the server will send; 0 or less when it doesn't say,
// which downloads in one stream from the start every time.
func Fetch(ctx context.Context, url string, expected int64, path string, progress func(int64)) error {
	if err := paths.ProtectDir(filepath.Dir(path)); err != nil {
		return err
	}
	if expected <= 0 {
		return fetchUnsized(ctx, url, path, progress)
	}
	if expected >= parallelMin {
		err := fetchRanges(ctx, url, expected, path, progress)
		if !errors.Is(err, errNoRanges) {
			return err
		}
	}
	return fetchStream(ctx, url, expected, path, progress)
}

func fetchStream(ctx context.Context, url string, expected int64, path string, progress func(int64)) error {
	downloaded := paths.PartialBytes(path, expected)
	if downloaded == expected {
		progress(downloaded)
		return nil
	}
	client := &http.Client{Timeout: 6 * time.Hour}
	req, err := newRequest(ctx, url)
	if err != nil {
		return err
	}
	if downloaded > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", downloaded))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		os.Remove(path)
		return fmt.Errorf("the download could not be resumed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	resumed := downloaded > 0 && resp.StatusCode == http.StatusPartialContent
	if resumed {
		cr, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil || cr.start != downloaded || cr.end+1 != expected || cr.total != expected {
			return fmt.Errorf("the download returned an invalid Content-Range")
		}
	} else if downloaded > 0 {
		// A 200 response ignored Range and carries the whole artifact.
		downloaded = 0
	}
	if resp.ContentLength >= 0 && resp.ContentLength != expected-downloaded {
		return fmt.Errorf("the download had an unexpected size")
	}
	flags := os.O_CREATE | os.O_WRONLY
	if resumed {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return fmt.Errorf("could not save download: %w", err)
	}
	progress(downloaded)
	buf := make([]byte, 1<<20)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if downloaded+int64(n) > expected {
				file.Close()
				return fmt.Errorf("the download exceeded its expected size")
			}
			if _, err := file.Write(buf[:n]); err != nil {
				file.Close()
				return fmt.Errorf("could not save download: %w", err)
			}
			downloaded += int64(n)
			progress(downloaded)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			return fmt.Errorf("download stopped: %w", readErr)
		}
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("could not finish download: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("could not finish download: %w", err)
	}
	if downloaded != expected {
		return fmt.Errorf("the download ended before its expected size")
	}
	return nil
}

type contentRange struct {
	start, end, total int64
}

func parseContentRange(value string) (contentRange, error) {
	var cr contentRange
	rest, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return cr, fmt.Errorf("invalid Content-Range")
	}
	rangePart, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return cr, fmt.Errorf("invalid Content-Range")
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return cr, fmt.Errorf("invalid Content-Range")
	}
	var err error
	if cr.start, err = strconv.ParseInt(startPart, 10, 64); err != nil {
		return cr, err
	}
	if cr.end, err = strconv.ParseInt(endPart, 10, 64); err != nil {
		return cr, err
	}
	if cr.total, err = strconv.ParseInt(totalPart, 10, 64); err != nil {
		return cr, err
	}
	return cr, nil
}

// The size url will send, from a HEAD that follows redirects (Hugging Face
// answers from its CDN).
func Size(ctx context.Context, url string) (int64, error) {
	req, err := newRequest(ctx, url)
	if err != nil {
		return 0, err
	}
	req.Method = http.MethodHead
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach %s: %w", req.URL.Host, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("%s answered HTTP %d", url, resp.StatusCode)
	}
	return resp.ContentLength, nil
}

func fetchUnsized(ctx context.Context, url, path string, progress func(int64)) error {
	req, err := newRequest(ctx, url)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 6 * time.Hour}).Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("could not save download: %w", err)
	}
	var done int64
	_, err = io.Copy(file, io.TeeReader(resp.Body, progressWriter(func(n int) {
		done += int64(n)
		progress(done)
	})))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download stopped: %w", err)
	}
	return nil
}

type progressWriter func(int)

func (w progressWriter) Write(p []byte) (int, error) {
	w(len(p))
	return len(p), nil
}

// A GET that carries the Hugging Face token to huggingface.co itself, never
// elsewhere. Its CDN redirects go to other domains, and net/http drops
// Authorization on a redirect that leaves the original domain.
func newRequest(ctx context.Context, url string) (*http.Request, error) {
	url, err := paths.HFMirrorURL(url)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token := paths.HFToken(); token != "" && paths.IsHFHost(req.URL.Host) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func fetchRanges(ctx context.Context, url string, expected int64, path string, progress func(int64)) error {
	state, resuming := paths.ReadPartRanges(path)
	if !resuming {
		// A single-stream partial keeps its prefix; the rest splits.
		start := paths.PartialBytes(path, expected)
		if start == expected {
			progress(expected)
			return nil
		}
		state = planRanges(start, expected)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("could not save download: %w", err)
	}
	defer file.Close()
	if err := file.Truncate(expected); err != nil {
		return fmt.Errorf("could not save download: %w", err)
	}
	if err := paths.WritePartRanges(path, state); err != nil {
		return err
	}

	var mu sync.Mutex
	total := func() int64 {
		var n int64
		for _, r := range state.Ranges {
			n += r.Done
		}
		return n
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(state.Ranges))
	var wg sync.WaitGroup
	for i := range state.Ranges {
		wg.Add(1)
		go func(r *paths.PartRange) {
			defer wg.Done()
			// Ranges report under the lock, so progress sees consistent
			// counts and never runs twice at once.
			err := fetchRange(ctx, url, expected, file, r, &mu, func() {
				mu.Lock()
				defer mu.Unlock()
				progress(total())
			})
			if err != nil {
				cancel()
			}
			errs <- err
		}(&state.Ranges[i])
	}
	// The sidecar is what makes a stopped download resumable; keep it current.
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				mu.Lock()
				paths.WritePartRanges(path, state)
				mu.Unlock()
			}
		}
	}()
	wg.Wait()
	cancel()
	<-saved
	close(errs)
	var first error
	for err := range errs {
		if err != nil && (first == nil || errors.Is(first, context.Canceled)) {
			first = err
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if errors.Is(first, errNoRanges) && total() == 0 {
		file.Close()
		os.Remove(path)
		os.Remove(paths.PartRangesPath(path))
		return errNoRanges
	}
	if first != nil {
		paths.WritePartRanges(path, state)
		return first
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("could not finish download: %w", err)
	}
	os.Remove(paths.PartRangesPath(path))
	progress(expected)
	return nil
}

// Splits [start, end) into up to maxRanges ranges of at least minRange; a
// prefix already on disk counts as one finished range.
func planRanges(start, end int64) *paths.PartRanges {
	state := &paths.PartRanges{}
	if start > 0 {
		state.Ranges = append(state.Ranges, paths.PartRange{Start: 0, End: start, Done: start})
	}
	n := min(maxRanges, max(1, (end-start)/minRange))
	size := (end - start + n - 1) / n
	for at := start; at < end; at += size {
		state.Ranges = append(state.Ranges, paths.PartRange{Start: at, End: min(at+size, end)})
	}
	return state
}

// One range, retried a few times on transient failures. Writes land at the
// range's own offset; Done only counts bytes that reached the file.
func fetchRange(ctx context.Context, url string, expected int64, file *os.File, r *paths.PartRange, mu *sync.Mutex, report func()) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		mu.Lock()
		from := r.Start + r.Done
		mu.Unlock()
		if from >= r.End {
			return nil
		}
		if err = fetchRangeOnce(ctx, url, expected, file, r, from, mu, report); err == nil || errors.Is(err, errNoRanges) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func fetchRangeOnce(ctx context.Context, url string, expected int64, file *os.File, r *paths.PartRange, from int64, mu *sync.Mutex, report func()) error {
	req, err := newRequest(ctx, url)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, r.End-1))
	resp, err := (&http.Client{Timeout: 6 * time.Hour}).Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return errNoRanges
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	cr, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || cr.start != from || cr.end != r.End-1 || cr.total != expected {
		return fmt.Errorf("the download returned an invalid Content-Range")
	}
	buf := make([]byte, 1<<20)
	at := from
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if at+int64(n) > r.End {
				return fmt.Errorf("the download exceeded its expected size")
			}
			if _, err := file.WriteAt(buf[:n], at); err != nil {
				return fmt.Errorf("could not save download: %w", err)
			}
			at += int64(n)
			mu.Lock()
			r.Done = at - r.Start
			mu.Unlock()
			report()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("download stopped: %w", readErr)
		}
	}
	if at != r.End {
		return fmt.Errorf("the download ended before its expected size")
	}
	return nil
}
