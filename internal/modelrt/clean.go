package modelrt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Reclaim disk: interrupted downloads, stale staging, tmp writes.
// `-all` also removes every installed model and the engine.
func Clean(all bool) error {
	root := paths.Home()
	var freed int64
	var removed []string
	reap := func(path string) {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if err := os.Remove(path); err == nil {
			freed += info.Size()
			removed = append(removed, path)
		}
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Built runtimes are huge and never hold fornax's own .part files.
			switch path {
			case kevSrcDir(root), layaSrcDir(root):
				return filepath.SkipDir
			}
			if filepath.Dir(path) == paths.EnginesDir(root) {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.Base(path)
		junk := strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".part.ranges") || strings.HasSuffix(name, ".tmp-write") || name == "server.log"
		if !junk {
			return nil
		}
		// A file touched moments ago is probably a live download.
		if time.Since(info.ModTime()) < 10*time.Second {
			return nil
		}
		reap(path)
		return nil
	})
	// Staging dirs a crashed pull left: `.engine-*`/`.ckpt-*` at home root
	// and `src.staging` under each python runtime. A dir touched moments ago
	// is likely live.
	for _, parent := range []string{root, kevRoot(root), layaRoot(root)} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			staging := entry.IsDir() &&
				(strings.HasPrefix(name, ".engine-") || strings.HasPrefix(name, ".ckpt-") || name == "src.staging")
			if !staging {
				continue
			}
			dir := filepath.Join(parent, name)
			if time.Since(dirFresh(dir)) < 10*time.Second {
				continue
			}
			freed += DirSize(dir)
			if os.RemoveAll(dir) == nil {
				removed = append(removed, dir+"/")
			}
		}
	}
	if all {
		for _, spec := range AllSpecs(root) {
			dir := paths.ModelDir(root, spec)
			if _, err := os.Stat(dir); err == nil {
				freed += DirSize(dir)
				if os.RemoveAll(dir) == nil {
					removed = append(removed, dir+"/")
				}
			}
		}
		_, kevErr := os.Stat(kevRoot(root))
		for _, dir := range []string{filepath.Join(root, "engine"), kevRoot(root), layaRoot(root)} {
			freed += DirSize(dir)
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			if err := os.RemoveAll(dir); err == nil {
				removed = append(removed, dir+"/")
			}
		}
		// kev's Qwen base lives in the shared HF cache — not ours to delete.
		if kevErr == nil {
			fmt.Println("note: the shared Hugging Face cache (~/.cache/huggingface) is left alone")
		}
		os.Remove(filepath.Join(root, CustomFile))
	}
	if len(removed) == 0 {
		fmt.Println(ui.Dim("nothing to clean"))
		return nil
	}
	for _, path := range removed {
		fmt.Printf("  %s %s\n", ui.Dim("−"), strings.TrimPrefix(path, root+string(os.PathSeparator)))
	}
	fmt.Printf("freed %s\n", ui.Bold(ui.HumanSize(freed)))
	return nil
}

// The newest mtime anywhere in a tree — a staging dir being actively
// unpacked keeps refreshing this.
func dirFresh(dir string) time.Time {
	fresh := time.Time{}
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.ModTime().After(fresh) {
			fresh = info.ModTime()
		}
		return nil
	})
	return fresh
}

func DirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
