package studio

// Studio's image and video kinds: one sd-cli run per job, its output
// folded into live progress (step, rate, phase) as it streams.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
)

func (s *studio) sdKind(kind catalog.Modality, ext string) studioKind {
	return studioKind{
		modelKind: kind,
		ext:       ext,
		validate: func(req *studioRequest) error {
			switch {
			case req.Width < 64 || req.Height < 64 || req.Width > 4096 || req.Height > 4096 || req.Width%16 != 0 || req.Height%16 != 0:
				return fmt.Errorf("size %dx%d is not a multiple of 16 between 64 and 4096", req.Width, req.Height)
			case req.Steps < 1 || req.Steps > 150:
				return fmt.Errorf("steps must be 1–150, got %d", req.Steps)
			case kind == catalog.Video && (req.Frames < 1 || req.Frames > 241):
				return fmt.Errorf("frames must be 1–241, got %d", req.Frames)
			case kind == catalog.Video && len(req.Refs) > 1:
				return fmt.Errorf("a video starts from one image, got %d", len(req.Refs))
			case kind == catalog.Video && len(req.Refs) == 1 && !studioImagePattern.MatchString(req.Refs[0]):
				return fmt.Errorf("a video starts from a PNG, JPEG or WebP image")
			}
			if kind != catalog.Video {
				req.Frames = 0
			}
			return nil
		},
		generate: s.sdGenerate,
	}
}

var (
	// `|=====>      | 3/20 - 13.44s/it` — sampling. Loading bars use MB/s.
	sdStepPattern   = regexp.MustCompile(`\|\s*(\d+)/(\d+) - ([\d.]+)(s/it|it/s)`)
	sdSampledMarker = "sampling completed"
	sdDoneMarker    = "generate_image completed"

	studioImagePattern = regexp.MustCompile(`\.(png|jpg|webp)$`)
)

// Where sd-cli redraws a job's preview. Video previews are animated WebP,
// which an <img> plays: sd-cli would turn a .png path into .avi for more
// than four frames (examples/cli/main.cpp), and browsers can't show AVI.
func (s *studio) previewPath(item *studioItem) string {
	ext := ".png"
	if item.Kind == "video" {
		ext = ".webp"
	}
	return filepath.Join(s.dir, "previews", item.ID+ext)
}

// Folds one line of sd-cli output into the job's progress.
func (s *studio) progress(job *studioJob, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case sdStepPattern.MatchString(line):
		m := sdStepPattern.FindStringSubmatch(line)
		step, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		rate, _ := strconv.ParseFloat(m[3], 64)
		if m[4] == "it/s" && rate > 0 {
			rate = 1 / rate
		}
		// The first step carries the warm-up; later ones say what a step costs.
		if step > 1 || job.StepSeconds == 0 {
			job.StepSeconds = rate
		}
		job.Phase, job.Step, job.Total = "sampling", step, total
		if info, err := os.Stat(s.previewPath(&job.studioItem)); err == nil && info.ModTime().After(job.previewStamp) {
			job.previewStamp = info.ModTime()
			job.Preview++
		}
	case strings.Contains(line, sdSampledMarker):
		job.Phase, job.sampled = "decoding", time.Now()
	case strings.Contains(line, sdDoneMarker):
		if !job.sampled.IsZero() {
			job.DecodeSeconds = time.Since(job.sampled).Seconds()
		}
		job.Phase = "saving"
	default:
		return
	}
	s.notifyLocked()
}

// Verify the pinned files, then one sd-cli run whose output streams through
// progress() and into studio/last.log.
func (s *studio) sdGenerate(ctx context.Context, job *studioJob) error {
	spec := modelrt.Model(job.Model)
	if spec == nil {
		return fmt.Errorf("%s is no longer installed", job.Model)
	}
	root, eng, err := modelrt.PrepareSD(ctx, spec)
	if err != nil {
		return err
	}
	s.setPhase(job, "loading")
	extra := []string{
		"-W", strconv.Itoa(job.Width), "-H", strconv.Itoa(job.Height),
		"--steps", strconv.Itoa(job.Steps),
	}
	if job.Kind == "video" {
		extra = append(extra, "--video-frames", strconv.Itoa(job.Frames))
	}
	// Models with a latent projection get a live preview; the rest warn
	// once per step into the log and carry on.
	extra = append(extra, "--preview", "proj", "--preview-path", s.previewPath(&job.studioItem))
	if job.Negative != "" {
		extra = append(extra, "-n", job.Negative)
	}
	// Video models take a start frame (-i, image-to-video); image models
	// take references to edit from (-r).
	refFlag := "-r"
	if job.Kind == "video" {
		refFlag = "-i"
	}
	for _, ref := range job.Refs {
		path := s.refPath(ref)
		if path == "" {
			return fmt.Errorf("reference %s is gone", ref)
		}
		extra = append(extra, refFlag, path)
	}
	cmd, _, err := modelrt.SDCommand(ctx, root, eng, spec, job.Prompt, s.outputPath(&job.studioItem), job.Seed, extra)
	if err != nil {
		return err
	}
	if err := s.runLogged(ctx, job, s.progress, cmd.Run, func(w io.Writer) { cmd.Stdout, cmd.Stderr = w, w }); err != nil {
		return err
	}
	if info, err := os.Stat(s.outputPath(&job.studioItem)); err != nil || info.Size() == 0 {
		return fmt.Errorf("sd-cli finished without writing a file: see %s", filepath.Join(s.dir, "last.log"))
	}
	return nil
}

// Runs a child whose combined output feeds progress line by line and lands
// in studio/last.log; a failure carries the last lines that weren't
// progress bars.
func (s *studio) runLogged(ctx context.Context, job *studioJob, progress func(*studioJob, string), run func() error, attach func(io.Writer)) error {
	logFile, err := os.OpenFile(filepath.Join(s.dir, "last.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	reader, writer := io.Pipe()
	attach(writer)
	tail := &lineTail{max: 8}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(io.TeeReader(reader, logFile))
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		scanner.Split(scanTerminalLines)
		for scanner.Scan() {
			line := scanner.Text()
			progress(job, line)
			if !strings.Contains(line, "|") {
				tail.add(line)
			}
		}
		io.Copy(io.Discard, reader)
	}()
	err = run()
	writer.Close()
	<-scanned
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, tail.String())
	}
	return nil
}

// Progress bars redraw with \r, so a carriage return ends a line as much as
// a newline does.
func scanTerminalLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, bytes.TrimSpace(data[:i]), nil
	}
	if atEOF && len(data) > 0 {
		return len(data), bytes.TrimSpace(data), nil
	}
	return 0, nil, nil
}

type lineTail struct {
	max   int
	lines []string
}

func (t *lineTail) add(line string) {
	if line = strings.TrimSpace(line); line == "" {
		return
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[1:]
	}
}

func (t *lineTail) String() string {
	if len(t.lines) == 0 {
		return "no output"
	}
	return strings.Join(t.lines, "\n")
}
