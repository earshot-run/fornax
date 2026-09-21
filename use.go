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
	if spec.rt == runtimeApple {
		return withApple(ctx, root, spec, fn)
	}
	key, err := ensureKey(root)
	if err != nil {
		return err
	}
	if contains(servedModels(spec.port, key), spec.id) {
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("reusing %s on :%d", spec.id, spec.port)))
		return fn(endpointURL(spec.port), key)
	}
	if err := rehash(root, spec); err != nil {
		return err
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
	loading := spin("loading " + spec.id)
	if err := waitReady(ctx, exited, port, spec.id, key, true); err != nil {
		loading.stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.stop("")
	return fn(endpointURL(port), key)
}

// The kev path through withServer: python env first, then kev.serve.
// kev speaks /v1/systemone, not chat completions — no API key either.
func withKev(ctx context.Context, root string, spec *modelSpec, fn func(url, key string) error) error {
	if contains(servedModels(spec.port, ""), kevAlias) {
		fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("reusing %s on :%d", spec.id, spec.port)))
		return fn(endpointURL(spec.port), "")
	}
	bar := newProgress("kev runtime", kevSource.bytes)
	if err := ensureKevRuntime(ctx, root, bar.set); err != nil {
		return err
	}
	if err := rehash(root, spec); err != nil {
		return err
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
	loading := spin("loading " + spec.id + " (first run downloads the base model)")
	if err := waitReady(ctx, exited, port, kevAlias, "", false); err != nil {
		loading.stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.stop("")
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

// A multi-turn REPL with history until /exit, /quit or Ctrl-D. A non-nil
// voice reads each reply aloud (`chat -speak`).
func runChat(ctx context.Context, spec *modelSpec, eng *engineSpec, voice *modelSpec) error {
	return withServer(ctx, spec, eng, func(url, key string) error {
		fmt.Fprintf(os.Stderr, "%s\n", dim(spec.name+" — type a message, /exit to leave, /clear to forget"))
		var history []message
		reader := bufio.NewReader(os.Stdin)
		interactive := isTTY(os.Stdin)
		for {
			if interactive {
				fmt.Print(cyan("› "))
			}
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			line = strings.TrimSpace(line)
			// A final line without a newline still gets answered; the next
			// read hits EOF and leaves.
			if err == io.EOF && line == "" {
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
				fmt.Fprintln(os.Stderr, dim("history cleared"))
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
			if voice != nil {
				if err := speakText(ctx, voice, reply.Text); err != nil {
					fmt.Fprintf(os.Stderr, "%s\n", dim("speech failed: "+err.Error()))
				}
			}
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
		reply, err := chatStream(ctx, url, key, spec.id, msgs, -1,
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
		reply, err := chatStream(ctx, url, key, spec.id, msgs, -1,
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
		fmt.Printf("%s %s — %q in %.1fs\n", green("✓"), bold(spec.id),
			strings.TrimSpace(reply.Text), elapsed.Seconds())
		if reply.Timings != nil {
			if v, ok := reply.Timings["predicted_per_second"].(float64); ok && v > 0 {
				fmt.Printf("    generate  %.0f tok/s\n", v)
			}
			if v, ok := reply.Timings["prompt_per_second"].(float64); ok && v > 0 {
				fmt.Printf("    prompt    %.0f tok/s\n", v)
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
	fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("llama-bench on %s (pp512 / tg128)", spec.id)))
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
	for _, spec := range allSpecs(root) {
		alias, key := spec.id, cfg.APIKey
		if spec.rt == runtimeKev {
			alias, key = kevAlias, ""
		}
		if contains(servedModels(spec.port, key), alias) {
			any = true
			fmt.Printf("%s %-16s %s :%d\n", markOK(), spec.id, dim("serving"), spec.port)
		}
	}
	if !any {
		fmt.Println(dim("nothing is serving — `fornax run <model>` starts one"))
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
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Built runtimes are huge and never hold fornax's own .part files.
			switch path {
			case kevSrcDir(root), engineDir(root):
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.Base(path)
		junk := strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp-write") || name == "server.log"
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
	// and `kev/src.staging`. A dir touched moments ago is likely live.
	for _, parent := range []string{root, kevRoot(root)} {
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
			freed += dirSize(dir)
			if os.RemoveAll(dir) == nil {
				removed = append(removed, dir+"/")
			}
		}
	}
	if all {
		for _, spec := range allSpecs(root) {
			dir := modelDir(root, spec)
			if _, err := os.Stat(dir); err == nil {
				freed += dirSize(dir)
				if os.RemoveAll(dir) == nil {
					removed = append(removed, dir+"/")
				}
			}
		}
		_, kevErr := os.Stat(kevRoot(root))
		for _, dir := range []string{filepath.Join(root, "engine"), kevRoot(root)} {
			freed += dirSize(dir)
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			if err := os.RemoveAll(dir); err == nil {
				removed = append(removed, dir+"/")
			}
		}
		// The Qwen3 base lives in the shared HF cache — not ours to delete.
		if kevErr == nil {
			fmt.Println("note: the shared Hugging Face cache (~/.cache/huggingface) is left alone")
		}
		os.Remove(filepath.Join(root, customFile))
	}
	if len(removed) == 0 {
		fmt.Println(dim("nothing to clean"))
		return nil
	}
	for _, path := range removed {
		fmt.Printf("  %s %s\n", dim("−"), strings.TrimPrefix(path, root+string(os.PathSeparator)))
	}
	fmt.Printf("freed %s\n", bold(humanSize(freed)))
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
