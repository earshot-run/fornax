package modelrt

// A warm stable-diffusion.cpp: sd-server holds one model's weights between
// generations so only the first pays for loading them. It ships beside
// sd-cli in every upstream build and speaks the native async API
// (/sdcpp/v1, examples/server/api.md upstream): submit a job, poll it, read
// the file back as base64. Each request merges over the defaults the server
// was started with, so a model's saved engine args still apply.
//
// sd-server has no API key; like kev it answers only on loopback.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
)

const sdJobPoll = 250 * time.Millisecond

// One sd-server with one model loaded.
type SDServer struct {
	Spec   *catalog.Spec
	base   string
	cmd    *exec.Cmd
	exited chan error
	client *http.Client
}

// What one generation asks for; zero fields keep the server's defaults.
type SDRequest struct {
	Prompt   string
	Negative string
	Width    int
	Height   int
	Steps    int
	Frames   int
	Seed     int64
	// Image models edit from Refs; video models start from Init.
	Refs []string
	Init string
}

// sd-server beside the engine's sd-cli.
func sdServerBinary(root string, eng *catalog.EngineSpec) string {
	return paths.EngineBinary(root, eng, strings.Replace(eng.Binary, "sd-cli", "sd-server", 1))
}

// Spawn sd-server for spec and wait until its weights are loaded. Every
// line the server prints, loading and sampling progress included, goes to
// output. An error means no server is left running.
func StartSD(ctx context.Context, root string, eng *catalog.EngineSpec, spec *catalog.Spec, output func(line string)) (*SDServer, error) {
	binary := sdServerBinary(root, eng)
	if _, err := os.Stat(binary); err != nil {
		return nil, fmt.Errorf("the installed sd engine has no sd-server")
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return nil, err
	}
	args := append(sdModelArgs(root, spec), spec.Args...)
	args = append(args, "--listen-ip", "127.0.0.1", "--listen-port", fmt.Sprint(port))
	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Dir(binary)
	cmd.Env = engineEnv(root, binary)
	cmd.Stdin = nil
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start sd-server: %w", err)
	}
	go scanLines(reader, output)
	s := &SDServer{
		Spec:   spec,
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:    cmd,
		exited: make(chan error, 1),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
	go func() {
		err := cmd.Wait()
		writer.Close()
		s.exited <- err
		close(s.exited)
	}()
	if err := s.waitLoaded(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// sd-server loads the model before it listens, so any answer means loaded.
func (s *SDServer) waitLoaded(ctx context.Context) error {
	started := time.Now()
	for {
		if resp, err := s.client.Get(s.base + "/sdcpp/v1/capabilities"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Since(started) >= loadTimeout {
			return fmt.Errorf("sd-server did not load %s within %d minutes", s.Spec.ID, int(loadTimeout.Minutes()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case status := <-s.exited:
			return fmt.Errorf("sd-server exited before %s loaded (%v)", s.Spec.ID, status)
		case <-time.After(readyPoll):
		}
	}
}

// Closed once the server process is gone.
func (s *SDServer) Exited() <-chan error { return s.exited }

// Stop the server and wait for it to exit.
func (s *SDServer) Close() {
	s.cmd.Process.Kill()
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
	}
}

// One generation written to outPath. Cancelling ctx cancels the job on the
// server, which stays loaded for the next one.
func (s *SDServer) Generate(ctx context.Context, req SDRequest, outPath string) error {
	body, route, err := sdJobBody(s.Spec, req)
	if err != nil {
		return err
	}
	var submitted struct {
		ID string `json:"id"`
	}
	if err := s.call(http.MethodPost, "/sdcpp/v1/"+route, body, &submitted); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			s.call(http.MethodPost, "/sdcpp/v1/jobs/"+submitted.ID+"/cancel", nil, nil)
			return ctx.Err()
		case status := <-s.exited:
			return fmt.Errorf("sd-server exited mid-generation (%v)", status)
		case <-time.After(sdJobPoll):
		}
		var job sdJob
		if err := s.call(http.MethodGet, "/sdcpp/v1/jobs/"+submitted.ID, nil, &job); err != nil {
			return err
		}
		switch job.Status {
		case "completed":
			return job.write(outPath)
		case "failed", "cancelled":
			if job.Error != nil && job.Error.Message != "" {
				return fmt.Errorf("sd-server: %s", job.Error.Message)
			}
			return fmt.Errorf("sd-server: job %s", job.Status)
		}
	}
}

type sdJob struct {
	Status string `json:"status"`
	Result *struct {
		// Video: the whole container. Image: one entry per image.
		B64    string `json:"b64_json"`
		Images []struct {
			B64 string `json:"b64_json"`
		} `json:"images"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (job *sdJob) write(outPath string) error {
	encoded := ""
	if job.Result != nil {
		encoded = job.Result.B64
		if encoded == "" && len(job.Result.Images) > 0 {
			encoded = job.Result.Images[0].B64
		}
	}
	if encoded == "" {
		return errors.New("sd-server finished without a file")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("sd-server sent an unreadable file: %w", err)
	}
	return os.WriteFile(outPath, raw, 0o644)
}

// The native request for req, and the route it goes to.
func sdJobBody(spec *catalog.Spec, req SDRequest) (map[string]any, string, error) {
	body := map[string]any{"prompt": req.Prompt, "seed": req.Seed}
	if req.Negative != "" {
		body["negative_prompt"] = req.Negative
	}
	if req.Width > 0 && req.Height > 0 {
		body["width"], body["height"] = req.Width, req.Height
	}
	if req.Steps > 0 {
		body["sample_params"] = map[string]any{"sample_steps": req.Steps}
	}
	route := "img_gen"
	if spec.Kind == catalog.Video {
		route = "vid_gen"
		if req.Frames > 0 {
			body["video_frames"] = req.Frames
		}
		body["output_format"] = "webm"
	} else {
		body["output_format"] = "png"
	}
	if req.Init != "" {
		encoded, err := sdImage(req.Init)
		if err != nil {
			return nil, "", err
		}
		body["init_image"] = encoded
	}
	var refs []string
	for _, ref := range req.Refs {
		encoded, err := sdImage(ref)
		if err != nil {
			return nil, "", err
		}
		refs = append(refs, encoded)
	}
	if len(refs) > 0 {
		body["ref_images"] = refs
	}
	return body, route, nil
}

func sdImage(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func (s *SDServer) call(method, route string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.base+route, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("sd-server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("sd-server answered %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Progress bars redraw with \r, so a carriage return ends a line as much as
// a newline does.
func scanLines(r io.Reader, line func(string)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, bytes.TrimSpace(data[:i]), nil
		}
		if atEOF && len(data) > 0 {
			return len(data), bytes.TrimSpace(data), nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		if text := scanner.Text(); text != "" {
			line(text)
		}
	}
	io.Copy(io.Discard, r)
}
