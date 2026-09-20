package main

// The workbench half: every command that *uses* a model goes through
// withServer — reuse the model's server if it is already running, otherwise
// pull, verify, spawn on a scratch port, use it, and reap it on the way out.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Ports 7431+ are scratch space for one-shot commands; the catalog ports
// (7331+) belong to `run` so a permanent server is never disturbed.
const scratchPortBase = 7431

func withServer(ctx context.Context, spec *modelSpec, eng *engineSpec, fn func(url, key string) error) error {
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.rt == runtimeKev {
		return withKev(ctx, root, spec, fn)
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	key, err := ensureKey(root)
	if err != nil {
		return err
	}
	if contains(servedModels(spec.port, key), spec.id) {
		return fn(endpointURL(spec.port), key)
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnServer(root, eng, spec, port, contextWindow, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	fmt.Fprintf(os.Stderr, "starting %s…\n", spec.id)
	if err := waitReady(ctx, exited, port, spec.id, key, true); err != nil {
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	return fn(endpointURL(port), key)
}

// The kev path through withServer: python env first, then kev.serve.
// kev speaks /v1/systemone, not chat completions — no API key either.
func withKev(ctx context.Context, root string, spec *modelSpec, fn func(url, key string) error) error {
	bar := newProgress("kev runtime", kevSource.bytes)
	if err := ensureKevRuntime(ctx, root, bar.set); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	if contains(servedModels(spec.port, ""), kevAlias) {
		return fn(endpointURL(spec.port), "")
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnKev(root, spec, port, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	fmt.Fprintf(os.Stderr, "starting %s (first run downloads the base model)…\n", spec.id)
	if err := waitReady(ctx, exited, port, kevAlias, "", false); err != nil {
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	return fn(endpointURL(port), "")
}

// One prompt, one streamed reply. The prompt comes from the args or stdin.
func runAsk(ctx context.Context, spec *modelSpec, eng *engineSpec, prompt string) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		reply, err := chatStream(ctx, url, key, spec.id,
			[]message{textMessage("user", prompt)}, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		if err != nil {
			return err
		}
		if reply.Text == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		return nil
	})
}

// A multi-turn REPL with history until /exit, /quit or Ctrl-D.
func runChat(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		fmt.Fprintf(os.Stderr, "%s — type a message, /exit to leave\n", spec.name)
		var history []message
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("› ")
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			line = strings.TrimSpace(line)
			if err == io.EOF {
				fmt.Println()
				return nil
			}
			switch line {
			case "":
				continue
			case "/exit", "/quit":
				return nil
			case "/clear":
				history = history[:0]
				fmt.Fprintln(os.Stderr, "history cleared")
				continue
			}
			history = append(history, textMessage("user", line))
			reply, err := chatStream(ctx, url, key, spec.id, history, -1,
				func(token string) { fmt.Print(token) })
			fmt.Println()
			if err != nil {
				return err
			}
			history = append(history, textMessage("assistant", reply.Text))
		}
	})
}

// Describe an image. Vision models only.
func runSee(ctx context.Context, spec *modelSpec, eng *engineSpec, imagePath, question string) error {
	if spec.kind != modalVision {
		return fmt.Errorf("%s cannot see — pick a vision model (`list`)", spec.id)
	}
	part, err := imagePart(imagePath)
	if err != nil {
		return err
	}
	return withServer(ctx, spec, eng, func(url, key string) error {
		msgs := []message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": question},
				part,
			},
		}}
		_, err := chatStream(ctx, url, key, spec.id, msgs, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		return err
	})
}

// Transcribe or answer about an audio take. Audio models only.
func runHear(ctx context.Context, spec *modelSpec, eng *engineSpec, audioPath, question string) error {
	if spec.kind != modalAudio {
		return fmt.Errorf("%s cannot hear — pick an audio model (`list`)", spec.id)
	}
	part, err := audioPart(audioPath)
	if err != nil {
		return err
	}
	return withServer(ctx, spec, eng, func(url, key string) error {
		msgs := []message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": question},
				part,
			},
		}}
		_, err := chatStream(ctx, url, key, spec.id, msgs, -1,
			func(token string) { fmt.Print(token) })
		fmt.Println()
		return err
	})
}

// A smoke check with real numbers: does the model load and answer, and how
// fast. Reports prompt/generation speed from the server's own timings.
func runTest(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		started := time.Now()
		reply, err := chatOnce(ctx, url, key, spec.id,
			[]message{textMessage("user", "Reply with exactly: ok")}, 8)
		elapsed := time.Since(started)
		if err != nil {
			return err
		}
		if strings.TrimSpace(reply.Text) == "" {
			return fmt.Errorf("the model returned an empty reply")
		}
		fmt.Printf("%s: ok\n", spec.id)
		fmt.Printf("  reply:     %q in %.1fs\n", strings.TrimSpace(reply.Text), elapsed.Seconds())
		if reply.Timings != nil {
			if v, ok := reply.Timings["predicted_per_second"].(float64); ok && v > 0 {
				fmt.Printf("  generate:  %.0f tok/s\n", v)
			}
			if v, ok := reply.Timings["prompt_per_second"].(float64); ok && v > 0 {
				fmt.Printf("  prompt:    %.0f tok/s\n", v)
			}
		}
		return nil
	})
}

// The engine's own benchmark on the installed weights (pp512 / tg128 table).
func runBench(ctx context.Context, spec *modelSpec, eng *engineSpec) error {
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
	}
	bench := engineBinary(root, eng, eng.bench)
	if _, err := os.Stat(bench); err != nil {
		return fmt.Errorf("llama-bench is not in the pinned engine")
	}
	cmd := exec.CommandContext(ctx, bench, "-m", modelFinal(root, spec))
	cmd.Dir = filepath.Dir(bench)
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "server-home"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// What is serving right now, catalog ports only.
func runPs() error {
	root := home()
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	any := false
	for i := range models {
		spec := &models[i]
		served := servedModels(spec.port, cfg.APIKey)
		if len(served) > 0 {
			any = true
			fmt.Printf("%-16s :%d  serving [%s]\n", spec.id, spec.port, strings.Join(served, ", "))
		}
	}
	if !any {
		fmt.Println("nothing is serving — `fornax run <model>` starts one")
	}
	return nil
}

// Reclaim disk: interrupted downloads, stale staging, tmp writes.
// `-all` also removes every installed model and the engine.
func runClean(all bool) error {
	root := home()
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
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".part") && !strings.HasSuffix(path, ".tmp-write") {
			return nil
		}
		// A file touched moments ago is probably a live download.
		if time.Since(info.ModTime()) < 10*time.Second {
			return nil
		}
		reap(path)
		return nil
	})
	// Engine unpack staging dirs are `.engine-*` under home.
	if entries, err := os.ReadDir(filepath.Join(root, "engine")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".engine-") {
				dir := filepath.Join(root, "engine", entry.Name())
				if size := dirSize(dir); size > 0 {
					freed += size
				}
				if os.RemoveAll(dir) == nil {
					removed = append(removed, dir+"/")
				}
			}
		}
	}
	if all {
		for i := range models {
			dir := modelDir(root, &models[i])
			if _, err := os.Stat(dir); err == nil {
				freed += dirSize(dir)
				if os.RemoveAll(dir) == nil {
					removed = append(removed, dir+"/")
				}
			}
		}
		for _, dir := range []string{filepath.Join(root, "engine"), kevRoot(root)} {
			freed += dirSize(dir)
			if err := os.RemoveAll(dir); err == nil {
				removed = append(removed, dir+"/")
			}
		}
	}
	if len(removed) == 0 {
		fmt.Println("nothing to clean")
		return nil
	}
	for _, path := range removed {
		fmt.Printf("removed %s\n", strings.TrimPrefix(path, root+string(os.PathSeparator)))
	}
	fmt.Printf("freed %s\n", humanSize(freed))
	return nil
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
