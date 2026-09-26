package modelrt

// Every command that *uses* a model goes through WithServer — reuse the
// model's server if it is already running, otherwise pull, verify, spawn on
// a scratch port, use it, and reap it on the way out.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// Ports 7431+ are scratch space for one-shot commands; each model's own port
// (7331+) belongs to `run` so a permanent server is never disturbed.
const scratchPortBase = 7431

// Run fn against a server for spec: the model's own when it is already
// serving, otherwise one spawned on a scratch port for fn's span and reaped
// after. Every runtime ends in the same fn(url, key).
func WithServer(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, fn func(url, key string) error) error {
	root := paths.Home()
	if err := Pull(ctx, spec, eng); err != nil {
		return err
	}
	if spec.Runtime == catalog.Kev {
		return withKev(ctx, root, spec, fn)
	}
	if spec.Runtime == catalog.Laya {
		return withLaya(ctx, root, spec, fn)
	}
	if spec.Runtime == catalog.Apple {
		return withApple(ctx, root, spec, fn)
	}
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if IsServing(spec, key) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), key)
	}
	if err := Rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnServer(root, eng, spec, port, catalog.ContextWindow, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID)
	if err := waitReady(ctx, exited, port, spec.ID, key, true); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), key)
}

// The kev path through WithServer: python env first, then kev.serve.
// kev speaks /v1/systemone, not chat completions — no API key either.
func withKev(ctx context.Context, root string, spec *catalog.Spec, fn func(url, key string) error) error {
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if IsServing(spec, key) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), key)
	}
	bar := ui.NewProgress("kev runtime", kevSource.Bytes)
	if err := ensureKevRuntime(ctx, root, bar.Set); err != nil {
		return err
	}
	if err := Rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnKev(root, spec, port, key, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID + " (first run downloads the base model)")
	if err := waitReady(ctx, exited, port, kevAlias, key, false); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), key)
}

// The laya path through WithServer: python env first, then the embedded
// serve shim. Unlike kev it answers behind the loopback key.
func withLaya(ctx context.Context, root string, spec *catalog.Spec, fn func(url, key string) error) error {
	key, err := paths.EnsureKey(root)
	if err != nil {
		return err
	}
	if IsServing(spec, key) {
		fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("reusing %s on :%d", spec.ID, spec.Port)))
		return fn(paths.EndpointURL(spec.Port), key)
	}
	bar := ui.NewProgress("laya runtime", layaSource.Bytes)
	if err := ensureLayaRuntime(ctx, root, bar.Set); err != nil {
		return err
	}
	if err := Rehash(root, spec); err != nil {
		return err
	}
	port, err := freePort(scratchPortBase)
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "server.log")
	cmd, err := spawnLaya(root, spec, port, logPath)
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer killAndReap(cmd, exited)
	loading := ui.Spin("loading " + spec.ID)
	if err := waitReady(ctx, exited, port, spec.ID, key, true); err != nil {
		loading.Stop("")
		return fmt.Errorf("%w — server log: %s", err, logPath)
	}
	loading.Stop("")
	return fn(paths.EndpointURL(port), key)
}

// The engine's own benchmark on the installed weights (pp512 / tg128 table).
func Bench(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec) error {
	root := paths.Home()
	if err := Pull(ctx, spec, eng); err != nil {
		return err
	}
	if err := Rehash(root, spec); err != nil {
		return err
	}
	bench := paths.EngineBinary(root, eng, eng.Bench)
	if _, err := os.Stat(bench); err != nil {
		return fmt.Errorf("llama-bench is not in the pinned engine")
	}
	fmt.Fprintf(os.Stderr, "%s\n", ui.Dim(fmt.Sprintf("llama-bench on %s (pp512 / tg128)", spec.ID)))
	cmd := exec.CommandContext(ctx, bench, "-m", paths.ModelFinal(root, spec))
	cmd.Dir = filepath.Dir(bench)
	cmd.Env = engineEnv(root, bench)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
