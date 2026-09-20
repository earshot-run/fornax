package main

// kev support — a different kind of local model. Kev is jaredpalmer's
// Jev-style decision model: a LoRA adapter plus readout head on a Qwen3 base,
// served by a small FastAPI app (`python -m kev.serve`). It answers typed
// questions with calibrated probabilities over TypeSafe's /v1/systemone API —
// it does not chat, so it cannot run under llama-server.
//
// earshot-local pins the kev source tarball and each checkpoint tarball the
// same way it pins llama.cpp and GGUFs, then bootstraps a uv venv once.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The pinned kev source — a GitHub commit archive, byte-counted and hashed
// like every other artifact.
var kevSource = filePin{
	file:     "kev-86db6d92.tar.gz",
	revision: "86db6d924cee68fa9a1319d2c3e6010b9b233d60",
	bytes:    15_415_754,
	sha256:   "47ca0e64f1a830c475eac53063bd1dc54b89f8ba94b7ec915a32290d8ef0fc43",
}

const kevSourceURL = "https://github.com/jaredpalmer/kev/archive/86db6d924cee68fa9a1319d2c3e6010b9b233d60.tar.gz"
const kevAlias = "kev-latest"

func kevRoot(root string) string     { return filepath.Join(root, "kev") }
func kevSrcDir(root string) string   { return filepath.Join(kevRoot(root), "src") }
func kevPython(root string) string   { return filepath.Join(kevSrcDir(root), ".venv", "bin", "python") }
func kevSrcPart(root string) string  { return filepath.Join(kevRoot(root), kevSource.file+".part") }
func kevSrcFinal(root string) string { return filepath.Join(kevRoot(root), kevSource.file) }
func kevRuntimeReady(root string) bool {
	if _, err := os.Stat(kevPython(root)); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(kevRoot(root), receipt))
	return err == nil && strings.TrimSpace(string(data)) == kevSource.sha256
}

// The directory kev.serve should --run: the unpacked checkpoint dir holding
// head.pt, one level under models/<id>/.
func kevCkptDir(root string, spec *modelSpec) string {
	base := modelDir(root, spec)
	if _, err := os.Stat(filepath.Join(base, "head.pt")); err == nil {
		return base
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dir := filepath.Join(base, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "head.pt")); err == nil {
				return dir
			}
		}
	}
	return ""
}

// Fetch the pinned source, unpack it, build the venv with uv (the heavy part —
// torch and friends, a few GB on first run).
func ensureKevRuntime(ctx context.Context, root string, progress func(int64)) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("kev models need macOS or Linux (torch MPS/CUDA)")
	}
	if kevRuntimeReady(root) {
		return nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return fmt.Errorf("kev needs `uv` to build its python env — install it from https://docs.astral.sh/uv/")
	}
	if err := protectDir(kevRoot(root)); err != nil {
		return err
	}
	part := kevSrcPart(root)
	final := kevSrcFinal(root)
	if _, err := os.Stat(final); os.IsNotExist(err) {
		if info, err := os.Stat(part); err == nil && info.Size() > kevSource.bytes {
			os.Remove(part)
		}
		if err := fetch(ctx, kevSourceURL, kevSource.bytes, part, progress); err != nil {
			return err
		}
		if err := verify(part, kevSource.bytes, kevSource.sha256); err != nil {
			return err
		}
		if err := os.Rename(part, final); err != nil {
			return fmt.Errorf("could not install kev source: %w", err)
		}
	}
	src := kevSrcDir(root)
	staging := src + ".staging"
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := protectDir(staging); err != nil {
		return err
	}
	if err := unpackTarGz(final, staging); err != nil {
		return err
	}
	// The archive wraps everything in kev-<sha>/ — promote that dir.
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		os.RemoveAll(staging)
		return fmt.Errorf("the kev source archive has an unexpected layout")
	}
	os.RemoveAll(src)
	if err := os.Rename(filepath.Join(staging, entries[0].Name()), src); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("could not install kev source: %w", err)
	}
	os.RemoveAll(staging)

	fmt.Fprintf(os.Stderr, "building kev python env with uv (torch download, one time)…\n")
	sync := exec.CommandContext(ctx, uv, "sync", "--extra", "serve")
	sync.Dir = src
	sync.Env = inheritEnv()
	sync.Stdout = os.Stderr
	sync.Stderr = os.Stderr
	if err := sync.Run(); err != nil {
		return fmt.Errorf("uv sync failed: %w", err)
	}
	return atomicPrivate(filepath.Join(kevRoot(root), receipt), []byte(kevSource.sha256+"\n"))
}

// kev runs on the user's real environment — it downloads the Qwen3 base from
// Hugging Face into ~/.cache/huggingface on first load.
func inheritEnv() []string {
	home, _ := os.UserHomeDir()
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"PYTHONUNBUFFERED=1",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	for _, key := range []string{"HF_HOME", "HF_TOKEN", "UV_PYTHON", "UV_CACHE_DIR"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func spawnKev(root string, spec *modelSpec, port int, logPath string) (*exec.Cmd, error) {
	ckpt := kevCkptDir(root, spec)
	if ckpt == "" {
		return nil, fmt.Errorf("%s has no unpacked checkpoint (missing head.pt)", spec.id)
	}
	python := kevPython(root)
	args := []string{"-m", "kev.serve", "--run", ckpt, "--port", fmt.Sprintf("%d", port)}
	cmd := exec.Command(python, args...)
	cmd.Dir = kevSrcDir(root)
	cmd.Env = inheritEnv()
	if spec.dtype != "" {
		cmd.Env = append(cmd.Env, "KEV_DTYPE="+spec.dtype)
	}
	cmd.Stdin = nil
	if logPath != "" {
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("could not open %s: %w", logPath, err)
		}
		cmd.Stdout = log
		cmd.Stderr = log
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start kev.serve: %w", err)
	}
	return cmd, nil
}

// ── /v1/systemone ────────────────────────────────────────────────────────────

// A typed question in the request. Criteria is a list (score) or a map of
// label→description (choice); noul needs neither.
type kevQuestion struct {
	Kind         string
	Instructions string
	Options      []string
}

func (q *kevQuestion) json() map[string]any {
	body := map[string]any{"type": q.Kind, "instructions": q.Instructions}
	switch q.Kind {
	case "choice":
		criteria := map[string]any{}
		for _, opt := range q.Options {
			criteria[opt] = opt
		}
		body["criteria"] = criteria
	case "score":
		body["criteria"] = q.Options
	}
	return body
}

func kevSystemOne(ctx context.Context, url string, state string, questions map[string]*kevQuestion) (map[string]any, error) {
	qs := map[string]any{}
	for id, q := range questions {
		qs[id] = q.json()
	}
	body, _ := json.Marshal(map[string]any{
		"model": kevAlias, "state": state, "questions": qs,
	})
	resp, err := post(ctx, url+"/systemone", "", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apiError(resp)
	}
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	return parsed, nil
}

// Print the answers as compact probability lines.
func printKevAnswers(reply map[string]any, order []string) {
	answers, _ := reply["answers"].(map[string]any)
	if answers == nil {
		data, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Println(string(data))
		return
	}
	ids := order
	if len(ids) == 0 {
		for id := range answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		a, ok := answers[id].(map[string]any)
		if !ok {
			continue
		}
		switch a["type"] {
		case "noul":
			fmt.Printf("  %-14s noul %.2f\n", id+":", a["noul"])
		case "choice":
			fmt.Printf("  %-14s choice %q — %s\n", id+":", a["choice"], probList(a))
		case "score":
			fmt.Printf("  %-14s score %.2f — %s\n", id+":", a["score"], probList(a))
		}
	}
	if ms, ok := reply["latency_ms"].(float64); ok {
		fmt.Printf("  %.0f ms\n", ms)
	}
}

func probList(answer map[string]any) string {
	probs, _ := answer["probabilities"].(map[string]any)
	legend, _ := answer["legend"].(map[string]any)
	keys := make([]string, 0, len(probs))
	for k := range probs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := k
		if legend[k] != nil {
			label = fmt.Sprint(legend[k])
		}
		if p, ok := probs[k].(float64); ok {
			parts = append(parts, fmt.Sprintf("%s %.2f", label, p))
		}
	}
	return strings.Join(parts, "  ")
}

// A canned request for `test` and `bench`: three question shapes at once.
func kevProbeQuestions() (map[string]*kevQuestion, []string) {
	return map[string]*kevQuestion{
		"escalate": {Kind: "noul", Instructions: "Does this need urgent human attention?"},
		"team": {Kind: "choice", Instructions: "Which team should handle this?",
			Options: []string{"returns", "shipping", "billing"}},
		"mood": {Kind: "score", Instructions: "How frustrated is the customer?",
			Options: []string{"calm", "frustrated", "angry"}},
	}, []string{"escalate", "team", "mood"}
}

const kevProbeState = "Shoes arrived two weeks late and in the wrong size. Also I see two charges on my card."

// What a `run` prints for a kev model: it speaks TypeSafe's API, not chat
// completions, so the Earshot connect block does not apply.
func kevBlock(url string) string {
	return fmt.Sprintf(`TypeSafe SDK → client = TypeSafeClient(
  base_url: %s
  model:    %s
  api_key:  "local"   (kev has no auth — loopback only)`, url, kevAlias)
}

func runKevTest(ctx context.Context, spec *modelSpec) error {
	return withServer(ctx, spec, nil, func(url, _ string) error {
		qs, order := kevProbeQuestions()
		reply, err := kevSystemOne(ctx, url, kevProbeState, qs)
		if err != nil {
			return err
		}
		fmt.Printf("%s: ok\n", spec.id)
		printKevAnswers(reply, order)
		return nil
	})
}

// Median latency over a handful of identical requests — the prefix cache makes
// later ones representative of steady state.
func runKevBench(ctx context.Context, spec *modelSpec, runs int) error {
	return withServer(ctx, spec, nil, func(url, _ string) error {
		qs, _ := kevProbeQuestions()
		var lat []float64
		for i := 0; i < runs; i++ {
			reply, err := kevSystemOne(ctx, url, kevProbeState, qs)
			if err != nil {
				return err
			}
			if ms, ok := reply["latency_ms"].(float64); ok {
				lat = append(lat, ms)
				fmt.Printf("  run %d: %.0f ms\n", i+1, ms)
			}
		}
		if len(lat) == 0 {
			return fmt.Errorf("the server reported no latencies")
		}
		sort.Float64s(lat)
		fmt.Printf("%s: median %.0f ms over %d requests\n", spec.id, lat[len(lat)/2], len(lat))
		return nil
	})
}

// `judge` — ask a live kev model typed questions about a piece of state.
// --ask 'id|type|instructions|opt1|opt2|…'  (repeatable; noul takes no options)
// or --json <file|-> for a raw SystemOne request body.
func runJudge(ctx context.Context, spec *modelSpec, state string, asks []string, jsonPath string) error {
	return withServer(ctx, spec, nil, func(url, _ string) error {
		if jsonPath != "" {
			var raw []byte
			var err error
			if jsonPath == "-" {
				raw, err = io.ReadAll(os.Stdin)
			} else {
				raw, err = os.ReadFile(jsonPath)
			}
			if err != nil {
				return err
			}
			resp, err := post(ctx, url+"/systemone", "", raw)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return apiError(resp)
			}
			var parsed map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				return err
			}
			printKevAnswers(parsed, nil)
			return nil
		}
		questions := map[string]*kevQuestion{}
		var order []string
		for _, ask := range asks {
			parts := strings.Split(ask, "|")
			if len(parts) < 3 {
				return fmt.Errorf("--ask %q needs id|type|instructions[|options…]", ask)
			}
			q := &kevQuestion{Kind: parts[1], Instructions: parts[2], Options: parts[3:]}
			switch q.Kind {
			case "noul":
			case "choice", "score":
				if len(q.Options) < 2 {
					return fmt.Errorf("--ask %q: %s needs at least two options", ask, q.Kind)
				}
			default:
				return fmt.Errorf("--ask %q: type must be noul, choice or score", ask)
			}
			questions[parts[0]] = q
			order = append(order, parts[0])
		}
		if len(questions) == 0 {
			return fmt.Errorf("no questions — pass --ask 'id|noul|instructions' (or --json)")
		}
		reply, err := kevSystemOne(ctx, url, state, questions)
		if err != nil {
			return err
		}
		printKevAnswers(reply, order)
		return nil
	})
}
