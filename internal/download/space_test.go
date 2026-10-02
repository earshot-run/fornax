package download

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreeSpaceUsesDestinationAncestor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	free, err := FreeSpace(filepath.Join(root, "not-created", "models"))
	if err != nil || free < 0 {
		t.Fatalf("free space = %d, %v", free, err)
	}
	if _, err := os.Stat(filepath.Join(root, "not-created")); !os.IsNotExist(err) {
		t.Fatalf("capacity probe created a directory: %v", err)
	}
}

func TestSpaceCheckAllowsResumeAndUnknownCapacity(t *testing.T) {
	probe := func(string) (int64, error) { return 40, nil }
	if err := checkSpace("weights.part", 40, probe); err != nil {
		t.Fatal(err)
	}
	if err := checkSpace("weights.part", 41, probe); err == nil || !strings.Contains(err.Error(), "free space and retry") {
		t.Fatalf("insufficient capacity: %v", err)
	}
	if err := checkSpace("weights.part", 100, func(string) (int64, error) { return 0, errors.New("unavailable") }); err != nil {
		t.Fatalf("unknown capacity blocked download: %v", err)
	}
	if spaceBytes(math.MaxUint64, 4096) != math.MaxInt64 {
		t.Fatal("disk capacity overflowed")
	}
}

func TestLowSpaceDoesNotRequestOrDamagePartial(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	path := filepath.Join(root, "weights.part")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("low-space download made a network request")
	}))
	defer server.Close()
	err := Fetch(context.Background(), server.URL, math.MaxInt64, path, func(int64) {})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("download error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("partial file changed: %q, %v", data, err)
	}
}

func TestUnsizedResponseChecksAnnouncedLengthBeforeTruncate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORNAX_HOME", root)
	path := filepath.Join(root, "weights.part")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "9223372036854775807")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	err := Fetch(context.Background(), server.URL, 0, path, func(int64) {})
	if err == nil || !strings.Contains(err.Error(), "not enough disk space") {
		t.Fatalf("download error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("partial file changed: %q, %v", data, err)
	}
}
