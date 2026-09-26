package engine

// Package engine installs a pinned engine build — llama.cpp or
// stable-diffusion.cpp, any backend: download every archive to `.part`,
// verify, unpack into a staging dir, rename into place, write the receipt.
// The unpackers are exported because the kev tarball arrives the same way.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// progress sees the bytes fetched across every archive of the build.
func Ensure(ctx context.Context, root string, spec *catalog.EngineSpec, progress func(int64)) error {
	if paths.EngineInstalled(root, spec) {
		return nil
	}
	archives := []catalog.EnginePart{{URL: spec.URL, Bytes: spec.Bytes, SHA256: spec.SHA256, Archive: spec.Archive, Kind: spec.Kind}}
	archives = append(archives, spec.Parts...)
	var done int64
	for _, archive := range archives {
		part := paths.EnginePart(root, archive.Archive)
		offset := done
		if err := download.Fetch(ctx, archive.URL, archive.Bytes, part, func(n int64) { progress(offset + n) }); err != nil {
			return err
		}
		if err := download.Verify(part, archive.Bytes, archive.SHA256); err != nil {
			return err
		}
		done += archive.Bytes
	}
	if err := install(root, spec, archives); err != nil {
		return err
	}
	for _, archive := range archives {
		os.Remove(paths.EnginePart(root, archive.Archive))
	}
	return nil
}

func install(root string, spec *catalog.EngineSpec, archives []catalog.EnginePart) error {
	if err := paths.ProtectDir(root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".engine-")
	if err != nil {
		return fmt.Errorf("could not stage %s: %w", spec.Name, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(staging)
		}
	}()
	if err := unpack(archives[0], root, staging); err != nil {
		return err
	}
	binaryDir := filepath.Join(staging, filepath.Dir(filepath.FromSlash(spec.Binary)))
	for _, part := range archives[1:] {
		if err := unpackBeside(part, root, binaryDir); err != nil {
			return err
		}
	}
	if info, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(spec.Binary))); statErr != nil || info.IsDir() {
		return fmt.Errorf("the pinned %s archive is missing %s", spec.Name, spec.Binary)
	}
	finalDir := paths.EngineDir(root, spec)
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("could not replace managed %s: %w", spec.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return fmt.Errorf("could not install %s: %w", spec.Name, err)
	}
	if err := os.Rename(staging, finalDir); err != nil {
		return fmt.Errorf("could not install %s: %w", spec.Name, err)
	}
	cleanup = false
	return paths.WriteEngineReceipt(finalDir, spec.Receipt())
}

func unpack(archive catalog.EnginePart, root, into string) error {
	part := paths.EnginePart(root, archive.Archive)
	if archive.Kind == catalog.Zip {
		return UnpackZip(part, into)
	}
	return UnpackTarGz(part, into)
}

// A part's files land next to the binary, which is where the loader looks
// (the exe's own dir on Windows, LD_LIBRARY_PATH on Linux). An archive that
// wraps everything in one top-level directory is unwrapped first.
func unpackBeside(archive catalog.EnginePart, root, binaryDir string) error {
	scratch, err := os.MkdirTemp(root, ".engine-part-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err := unpack(archive, root, scratch); err != nil {
		return err
	}
	from := scratch
	if entries, err := os.ReadDir(scratch); err == nil && len(entries) == 1 && entries[0].IsDir() {
		from = filepath.Join(scratch, entries[0].Name())
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		destination := filepath.Join(binaryDir, entry.Name())
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("%s would overwrite %s from the engine itself", archive.Archive, entry.Name())
		}
		if err := os.Rename(filepath.Join(from, entry.Name()), destination); err != nil {
			return fmt.Errorf("could not unpack %s: %w", archive.Archive, err)
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
