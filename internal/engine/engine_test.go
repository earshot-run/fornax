package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeRelativeRejectsTraversal(t *testing.T) {
	for _, ok := range []string{"llama-b11060/llama-server", "./a", "a/b/c.so"} {
		if err := safeRelative(ok); err != nil {
			t.Errorf("safeRelative(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"../escape", "/absolute", "", "a/../../b", "a/../.."} {
		if err := safeRelative(bad); err == nil {
			t.Errorf("safeRelative(%q) succeeded, want error", bad)
		}
	}
}

func TestUnpackTarGzWritesFilesAndLinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "llama-b11060/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "llama-b11060/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: 4})
	tw.Write([]byte("bin!"))
	tw.WriteHeader(&tar.Header{Name: "llama-b11060/LICENSE", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	tw.Write([]byte("MIT!"))
	tw.WriteHeader(&tar.Header{Name: "llama-b11060/link", Typeflag: tar.TypeSymlink, Linkname: "llama-server"})
	tw.Close()
	gz.Close()

	root := t.TempDir()
	archive := filepath.Join(root, "engine.tar.gz")
	os.WriteFile(archive, buf.Bytes(), 0o600)
	into := filepath.Join(root, "staging")
	os.Mkdir(into, 0o700)
	if err := UnpackTarGz(archive, into); err != nil {
		t.Fatalf("UnpackTarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(into, "llama-b11060", "llama-server"))
	if err != nil || string(got) != "bin!" {
		t.Fatalf("binary = %q, %v", got, err)
	}
	// The umask clears the group and world bits under a private ~/.fornax,
	// so only the owner's execute bit is load-bearing here.
	info, err := os.Stat(filepath.Join(into, "llama-b11060", "llama-server"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary mode = %v, %v", info.Mode(), err)
	}
	plain, err := os.Stat(filepath.Join(into, "llama-b11060", "LICENSE"))
	if err != nil || plain.Mode().Perm()&0o111 != 0 {
		t.Fatalf("LICENSE mode = %v, %v", plain.Mode(), err)
	}
	link, err := os.Readlink(filepath.Join(into, "llama-b11060", "link"))
	if err != nil || link != "llama-server" {
		t.Fatalf("link = %q, %v", link, err)
	}
}

func TestUnpackZipWritesFlatRelease(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry, _ := zw.Create("llama-server.exe")
	entry.Write([]byte("bin!"))
	lib, _ := zw.Create("ggml.dll")
	lib.Write([]byte("dll!"))
	zw.Close()

	root := t.TempDir()
	archive := filepath.Join(root, "engine.zip")
	os.WriteFile(archive, buf.Bytes(), 0o600)
	into := filepath.Join(root, "staging")
	os.Mkdir(into, 0o700)
	if err := UnpackZip(archive, into); err != nil {
		t.Fatalf("UnpackZip: %v", err)
	}
	for _, name := range []string{"llama-server.exe", "ggml.dll"} {
		if _, err := os.Stat(filepath.Join(into, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestUnpackTarGzRefusesTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()

	root := t.TempDir()
	archive := filepath.Join(root, "engine.tar.gz")
	os.WriteFile(archive, buf.Bytes(), 0o600)
	into := filepath.Join(root, "staging")
	os.Mkdir(into, 0o700)
	if err := UnpackTarGz(archive, into); err == nil {
		t.Fatal("UnpackTarGz accepted a traversal path")
	}
}
