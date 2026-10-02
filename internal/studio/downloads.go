package studio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
)

const downloadJournal = "downloads.json"

type downloadStore struct {
	Version   int            `json:"version"`
	Downloads []*hubDownload `json:"downloads"`
}

// Save intent and transitions, not credentials, commands or a claim that the
// bytes are installed. Actual .part files remain owned by the shared pull path.
func (s *studio) saveDownloadsLocked() error {
	return paths.AtomicJSON(filepath.Join(s.dir, downloadJournal), downloadStore{Version: 1, Downloads: s.downloads})
}

func (s *studio) loadDownloads() error {
	data, err := os.ReadFile(filepath.Join(s.dir, downloadJournal))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var store downloadStore
	if err := json.Unmarshal(data, &store); err != nil || store.Version != 1 {
		return fmt.Errorf("studio/%s is unreadable or unsupported; move it aside to recover (model files are untouched)", downloadJournal)
	}
	for _, d := range store.Downloads {
		if d == nil || d.ID == "" || d.Ref == "" {
			return fmt.Errorf("studio/%s contains an invalid download", downloadJournal)
		}
		switch d.State {
		case "running", "interrupted":
			d.State, d.Error = "interrupted", "Studio stopped during this download. Resume to reuse partial files."
		case "done":
			var spec *catalog.Spec
			for _, candidate := range modelrt.AllSpecs(s.root) {
				if candidate.ID == d.Model {
					spec = candidate
					break
				}
			}
			if spec == nil || !modelrt.Installed(s.root, spec) {
				// Do not turn a stale receipt into installed UI after removal.
				continue
			}
		case "failed":
		default:
			return fmt.Errorf("studio/%s contains an unknown download state", downloadJournal)
		}
		d.files = map[string][2]int64{}
		s.downloads = append(s.downloads, d)
	}
	return nil
}

// Stop children during graceful shutdown, retaining their intent for the next
// studio. Explicit Cancel instead dismisses intent while preserving .part data.
func (s *studio) interruptDownloads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuttingDown = true
	for _, d := range s.downloads {
		if d.State == "running" {
			d.State, d.Error = "interrupted", "Studio stopped during this download. Resume to reuse partial files."
			d.cancel()
		}
	}
	if err := s.saveDownloadsLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "could not save interrupted downloads: %v\n", err)
	}
}
