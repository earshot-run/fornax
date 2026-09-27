package studio

// Studio's image and video kinds: jobs run on a warm sd-server (sdSlot),
// its output folded into live progress (step, rate, phase) as it streams.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	sdStepPattern = regexp.MustCompile(`\|\s*(\d+)/(\d+) - ([\d.]+)(s/it|it/s)`)
	// The text encoder is done; step 1 on a slow backend can take a minute.
	sdSamplingMarker = "generating image:"
	sdSampledMarker  = "sampling completed"
	sdDoneMarker     = "generate_image completed"

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
	case strings.Contains(line, sdSamplingMarker):
		job.Phase = "sampling"
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

// Install what the model needs, then generate on the warm sd-server,
// loading it first when it holds some other model or none. A model whose
// saved args sd-server won't start with runs through sd-cli instead.
func (s *studio) sdGenerate(ctx context.Context, job *studioJob) error {
	spec := modelrt.Model(job.Model)
	if spec == nil {
		return fmt.Errorf("%s is no longer installed", job.Model)
	}
	root, eng, err := modelrt.PrepareSD(ctx, spec)
	if err != nil {
		return err
	}
	var refs []string
	for _, ref := range job.Refs {
		path := s.refPath(ref)
		if path == "" {
			return fmt.Errorf("reference %s is gone", ref)
		}
		refs = append(refs, path)
	}
	server, err := s.sd.acquire(ctx, root, eng, spec, func() { s.setPhase(job, "loading") })
	if err != nil {
		return err
	}
	if server == nil {
		return s.sdGenerateOnce(ctx, job, root, eng, spec, refs)
	}
	req := modelrt.SDRequest{
		Prompt: job.Prompt, Negative: job.Negative,
		Width: job.Width, Height: job.Height, Steps: job.Steps, Frames: job.Frames, Seed: job.Seed,
	}
	// Video models take a start frame; image models take references to edit from.
	if job.Kind == "video" && len(refs) > 0 {
		req.Init = refs[0]
	} else {
		req.Refs = refs
	}
	s.sd.follow(func(line string) { s.progress(job, line) })
	defer s.sd.follow(nil)
	if err := server.Generate(ctx, req, s.outputPath(&job.studioItem)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: see %s", err, s.sd.logPath)
	}
	return nil
}

// One sd-cli run whose output streams through progress() and into
// studio/last.log, with a live preview for models that have a latent
// projection.
func (s *studio) sdGenerateOnce(ctx context.Context, job *studioJob, root string, eng *catalog.EngineSpec, spec *catalog.Spec, refs []string) error {
	s.setPhase(job, "loading")
	extra := []string{
		"-W", strconv.Itoa(job.Width), "-H", strconv.Itoa(job.Height),
		"--steps", strconv.Itoa(job.Steps),
	}
	if job.Kind == "video" {
		extra = append(extra, "--video-frames", strconv.Itoa(job.Frames))
	}
	// Models without one warm once per step into the log and carry on.
	extra = append(extra, "--preview", "proj", "--preview-path", s.previewPath(&job.studioItem))
	if job.Negative != "" {
		extra = append(extra, "-n", job.Negative)
	}
	refFlag := "-r"
	if job.Kind == "video" {
		refFlag = "-i"
	}
	for _, path := range refs {
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

// The warm sd-server between generations: one model at a time, let go
// after sdIdle without a job, when another model needs the GPU, or when the
// studio stops. Only the queue's worker acquires it, so jobs never overlap.
type sdSlot struct {
	// Tests swap it for a fake.
	start   func(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, output func(string)) (sdServer, error)
	idle    time.Duration
	logPath string

	mu     sync.Mutex
	cur    sdServer
	busy   bool
	used   time.Time
	closed bool
	// Models sd-server would not start with this session; they use sd-cli.
	cold map[string]bool
	// Where the server's output goes while a job runs.
	line func(string)
}

type sdServer interface {
	Generate(ctx context.Context, req modelrt.SDRequest, outPath string) error
	Exited() <-chan error
	Close()
	ID() string
}

type warmSD struct{ *modelrt.SDServer }

func (w warmSD) ID() string { return w.Spec.ID }

const sdIdle = 10 * time.Minute

func newSDSlot(dir string) *sdSlot {
	return &sdSlot{
		start: func(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, output func(string)) (sdServer, error) {
			server, err := modelrt.StartSD(ctx, root, eng, spec, output)
			if err != nil {
				return nil, err
			}
			return warmSD{server}, nil
		},
		idle:    sdIdle,
		logPath: filepath.Join(dir, "sd-server.log"),
		cold:    map[string]bool{},
	}
}

// The server holding spec, loaded now if need be (loading runs first).
// nil with no error means spec runs through sd-cli.
func (c *sdSlot) acquire(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, loading func()) (sdServer, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("studio is shutting down")
	}
	if c.cold[spec.ID] {
		c.mu.Unlock()
		return nil, nil
	}
	previous := c.cur
	if previous != nil && previous.ID() == spec.ID && alive(previous) {
		c.busy, c.used = true, time.Now()
		c.mu.Unlock()
		return previous, nil
	}
	c.cur = nil
	c.busy = true
	c.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	loading()
	server, err := c.startLogged(ctx, root, eng, spec)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.busy = false
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.cold[spec.ID] = true
		return nil, nil
	}
	if c.closed {
		server.Close()
		return nil, errors.New("studio is shutting down")
	}
	c.cur, c.used = server, time.Now()
	return server, nil
}

// Starts the server with its output in studio/sd-server.log and, while a
// job follows it, in that job's progress.
func (c *sdSlot) startLogged(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec) (sdServer, error) {
	log, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	server, err := c.start(ctx, root, eng, spec, func(line string) {
		fmt.Fprintln(log, line)
		c.mu.Lock()
		follow := c.line
		c.mu.Unlock()
		if follow != nil {
			follow(line)
		}
	})
	if err != nil {
		fmt.Fprintf(log, "fornax: %v — %s runs through sd-cli for the rest of this studio session\n", err, spec.ID)
		log.Close()
		return nil, err
	}
	go func() {
		<-server.Exited()
		log.Close()
	}()
	return server, nil
}

// Sends the server's output lines to line until follow(nil); ends the job.
func (c *sdSlot) follow(line func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.line = line
	if line == nil {
		c.busy, c.used = false, time.Now()
	}
}

func alive(server sdServer) bool {
	select {
	case <-server.Exited():
		return false
	default:
		return true
	}
}

// Lets the server go unless a job is on it; id "" matches any model.
func (c *sdSlot) unload(id string) {
	c.mu.Lock()
	server := c.cur
	if server == nil || c.busy || (id != "" && server.ID() != id) {
		c.mu.Unlock()
		return
	}
	c.cur = nil
	c.mu.Unlock()
	server.Close()
}

func (c *sdSlot) reapIdle() {
	c.mu.Lock()
	idle := c.cur != nil && !c.busy && time.Since(c.used) >= c.idle
	c.mu.Unlock()
	if idle {
		c.unload("")
	}
}

// Stops the server for good, a running job's included.
func (c *sdSlot) stop() {
	c.mu.Lock()
	c.closed = true
	server := c.cur
	c.cur = nil
	c.mu.Unlock()
	if server != nil {
		server.Close()
	}
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
