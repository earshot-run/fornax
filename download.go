package main

// Resumable, strictly pinned downloads. A `.part` file resumes via HTTP
// Range; nothing is "done" until byte count and SHA-256 both match the pin.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func fetch(ctx context.Context, url string, expected int64, path string, progress func(int64)) error {
	if err := protectDir(filepath.Dir(path)); err != nil {
		return err
	}
	downloaded := partialBytes(path, expected)
	if downloaded == expected {
		progress(downloaded)
		return nil
	}
	client := &http.Client{Timeout: 6 * time.Hour}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
				return fmt.Errorf("the download exceeded its pinned size")
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
		return fmt.Errorf("the download ended before its pinned size")
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

func verify(path string, expectedBytes int64, expectedSHA string) error {
	if len(expectedSHA) != 64 {
		return fmt.Errorf("the pinned digest is invalid")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not read downloaded artifact: %w", err)
	}
	if info.Size() != expectedBytes {
		os.Remove(path)
		return fmt.Errorf("the downloaded artifact had the wrong size")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not verify downloaded artifact: %w", err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	file.Close()
	if copyErr != nil {
		return fmt.Errorf("could not verify downloaded artifact: %w", copyErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA {
		os.Remove(path)
		return fmt.Errorf("the download did not pass its SHA-256 check")
	}
	return nil
}
