package main

// Installing the pinned llama.cpp release: download to `.part`, verify,
// unpack into a staging dir, rename into place, write the receipt.

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
)

const (
	maxEngineExpanded = 512 * 1024 * 1024
	maxEngineEntries  = 4_096
)

func ensureEngine(ctx context.Context, root string, spec *engineSpec, progress func(int64)) error {
	if engineInstalled(root, spec) {
		return nil
	}
	archive := enginePart(root, spec)
	if err := fetch(ctx, spec.url, spec.bytes, archive, progress); err != nil {
		return err
	}
	if err := verify(archive, spec.bytes, spec.sha256); err != nil {
		return err
	}
	return installEngine(root, spec, archive)
}

func installEngine(root string, spec *engineSpec, archive string) error {
	if err := protectDir(root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".engine-")
	if err != nil {
		return fmt.Errorf("could not stage llama.cpp: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(staging)
		}
	}()
	switch spec.kind {
	case archiveTarGz:
		err = unpackTarGz(archive, staging)
	case archiveZip:
		err = unpackZip(archive, staging)
	}
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(staging, filepath.FromSlash(spec.binary))); statErr != nil || info.IsDir() {
		return fmt.Errorf("the pinned llama.cpp archive is missing %s", spec.binary)
	}
	finalDir := engineDir(root)
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("could not replace managed llama.cpp: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return fmt.Errorf("could not install llama.cpp: %w", err)
	}
	if err := os.Rename(staging, finalDir); err != nil {
		return fmt.Errorf("could not install llama.cpp: %w", err)
	}
	cleanup = false
	if err := writeReceipt(finalDir, spec.sha256); err != nil {
		return err
	}
	os.Remove(archive)
	return nil
}

// An archive entry must stay inside the staging dir: plain relative names only.
func safeRelative(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return fmt.Errorf("the llama.cpp archive contains an unsafe path: %s", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return fmt.Errorf("the llama.cpp archive contains an unsafe path: %s", name)
		}
	}
	return nil
}

func unpackTarGz(archive, into string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("could not open llama.cpp archive: %w", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("could not read llama.cpp archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for index := 0; ; index++ {
		if index >= maxEngineEntries {
			return fmt.Errorf("the llama.cpp archive has too many entries")
		}
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("could not read llama.cpp archive: %w", err)
		}
		if err := safeRelative(header.Name); err != nil {
			return err
		}
		if seen[header.Name] {
			return fmt.Errorf("the llama.cpp archive repeats a path: %s", header.Name)
		}
		seen[header.Name] = true
		destination := filepath.Join(into, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("could not unpack llama.cpp: %w", err)
			}
		case tar.TypeReg:
			total += header.Size
			if total > maxEngineExpanded {
				return fmt.Errorf("the llama.cpp archive expands too large")
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return fmt.Errorf("could not unpack llama.cpp: %w", err)
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
				return fmt.Errorf("could not unpack llama.cpp: %w", err)
			}
			if err := os.Symlink(header.Linkname, destination); err != nil {
				return fmt.Errorf("could not unpack llama.cpp link: %w", err)
			}
		default:
			return fmt.Errorf("the llama.cpp archive contains an unsupported entry")
		}
	}
}

func unpackZip(archive, into string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("could not read llama.cpp archive: %w", err)
	}
	defer reader.Close()
	if len(reader.File) > maxEngineEntries {
		return fmt.Errorf("the llama.cpp archive has too many entries")
	}
	seen := map[string]bool{}
	var total int64
	for _, entry := range reader.File {
		if err := safeRelative(entry.Name); err != nil {
			return err
		}
		if seen[entry.Name] {
			return fmt.Errorf("the llama.cpp archive repeats a path: %s", entry.Name)
		}
		seen[entry.Name] = true
		destination := filepath.Join(into, filepath.FromSlash(entry.Name))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("could not unpack llama.cpp: %w", err)
			}
			continue
		}
		total += int64(entry.UncompressedSize64)
		if total > maxEngineExpanded {
			return fmt.Errorf("the llama.cpp archive expands too large")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("could not unpack llama.cpp: %w", err)
		}
		mode := entry.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		source, err := entry.Open()
		if err != nil {
			return fmt.Errorf("could not unpack llama.cpp: %w", err)
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
		return fmt.Errorf("could not unpack llama.cpp: %w", err)
	}
	copied, err := io.Copy(output, source)
	if err != nil {
		output.Close()
		return fmt.Errorf("could not unpack llama.cpp: %w", err)
	}
	if copied != size {
		output.Close()
		return fmt.Errorf("the llama.cpp archive ended early")
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return fmt.Errorf("could not finish unpacking: %w", err)
	}
	return output.Close()
}
