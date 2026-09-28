// Package mcp is `fornax mcp` — a newline-delimited JSON-RPC 2.0 MCP server
// on stdin/stdout, so MCP-aware agents (Claude Code, Cursor) can drive the
// local models: ask, see, hear, judge, rerank, embed, imagine, animate, say
// and list. stdout carries only protocol replies: every spinner,
// progress bar and engine line stays on stderr, and the two foreground
// helpers that would print a success line (modelrt.RunSay, RunSD) get
// os.Stdout redirected while they run. Requests run one at a time — a model
// load can take minutes and the scratch servers are spawned --parallel 1
// anyway.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
	"github.com/earshot-run/fornax/internal/ui"
)

// The MCP revision this server speaks; serverInfo.version is the build stamp
// the caller passes to Run.
const mcpProtocol = "2025-06-18"

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// Serve MCP on stdin/stdout until stdin ends.
func Run(ctx context.Context, version string) error {
	return run(ctx, version, os.Stdin, os.Stdout)
}

func run(ctx context.Context, version string, input io.ReadCloser, output io.Writer) error {
	lines := make(chan string)
	ended := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*openai.MaxBody)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		ended <- scanner.Err()
	}()
	out := bufio.NewWriter(output)
	for {
		var line string
		select {
		case <-ctx.Done():
			// Closing inherited stdin can itself wait for a blocked read on macOS.
			go input.Close()
			return ctx.Err()
		case err := <-ended:
			return err
		case line = <-lines:
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeMCP(out, mcpReply{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &mcpError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification — no reply
		}
		reply := mcpDispatch(ctx, &req, version)
		reply.ID = req.ID
		writeMCP(out, reply)
	}
}

func writeMCP(out *bufio.Writer, reply mcpReply) {
	data, err := json.Marshal(reply)
	if err != nil {
		return
	}
	out.Write(data)
	out.WriteByte('\n')
	out.Flush()
}

func mcpDispatch(ctx context.Context, req *mcpRequest, version string) mcpReply {
	reply := mcpReply{JSONRPC: "2.0"}
	result, rpcErr := mcpHandle(ctx, req.Method, req.Params, version)
	if rpcErr != nil {
		reply.Error = rpcErr
		return reply
	}
	data, err := json.Marshal(result)
	if err != nil {
		reply.Error = &mcpError{Code: -32603, Message: err.Error()}
		return reply
	}
	reply.Result = data
	return reply
}

func mcpHandle(ctx context.Context, method string, params json.RawMessage, version string) (any, *mcpError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fornax", "version": version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		return mcpCall(ctx, params)
	}
	return nil, &mcpError{Code: -32601, Message: "method not found"}
}

// A tool that fails still gets a result — isError:true keeps the JSON-RPC
// layer clean so one bad call never kills the loop.
func mcpCall(ctx context.Context, params json.RawMessage) (any, *mcpError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &mcpError{Code: -32602, Message: "tools/call wants {name, arguments}"}
	}
	text, err := mcpRunTool(ctx, call.Name, call.Arguments)
	isError := err != nil
	if isError {
		text = err.Error()
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}, nil
}

func mcpRunTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "ask":
		return mcpAsk(ctx, args)
	case "see":
		return mcpSee(ctx, args)
	case "hear":
		return mcpHear(ctx, args)
	case "embed":
		return mcpEmbed(ctx, args)
	case "imagine":
		return mcpImagine(ctx, args)
	case "animate":
		return mcpAnimate(ctx, args)
	case "say":
		return mcpSay(ctx, args)
	case "judge":
		return mcpJudge(ctx, args)
	case "rerank":
		return mcpRerank(ctx, args)
	case "models":
		return mcpList()
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func mcpArgs(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, v)
}

// The model to serve: the explicit id, or the first spec of a wanted kind —
// an installed one when there is a choice.
func mcpResolve(ctx context.Context, id string, kinds ...catalog.Modality) (*catalog.Spec, *catalog.EngineSpec, error) {
	if id == "" {
		spec := mcpDefaultSpec(paths.Home(), kinds...)
		if spec == nil {
			return nil, nil, fmt.Errorf("no %s model is saved — pull one or pass a model id", kinds[0])
		}
		id = spec.ID
	}
	return modelrt.Resolve(ctx, id)
}

func mcpDefaultSpec(root string, kinds ...catalog.Modality) *catalog.Spec {
	var fallback *catalog.Spec
	for _, spec := range modelrt.AllSpecs(root) {
		match := false
		for _, kind := range kinds {
			if spec.Kind == kind {
				match = true
			}
		}
		if !match {
			continue
		}
		if modelrt.Installed(root, spec) {
			return spec
		}
		if fallback == nil {
			fallback = spec
		}
	}
	return fallback
}

// runAsk/runSee/runHear all stream to stdout; the MCP path is the same
// WithServer + chat completion minus the printing.
func mcpChat(ctx context.Context, spec *catalog.Spec, eng *catalog.EngineSpec, msgs []openai.Message) (string, error) {
	var reply *openai.Reply
	err := modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		r, err := openai.Once(ctx, url, key, spec.ID, msgs, -1)
		if err != nil {
			return err
		}
		reply = r
		return nil
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(reply.Text) == "" {
		return "", fmt.Errorf("the model returned an empty reply")
	}
	return reply.Text, nil
}

// RunSay/RunSD end with a `wrote …` line on stdout. Under MCP that fd is
// protocol-only, so their payload line goes to stderr for the call's span —
// where the codebase sends status anyway.
func mcpMuteStdout(fn func() error) error {
	real := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = real }()
	return fn()
}

func mcpAsk(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("ask wants a prompt")
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Text, catalog.Vision, catalog.Audio)
	if err != nil {
		return "", err
	}
	if does, command, _ := modelrt.Instead(spec); does != "" {
		switch command {
		case "imagine", "animate", "say", "embed":
			return "", fmt.Errorf("%s %s, it does not chat — use the %s tool", spec.ID, does, command)
		}
		return "", fmt.Errorf("%s %s, it does not chat", spec.ID, does)
	}
	return mcpChat(ctx, spec, eng, []openai.Message{openai.TextMessage("user", in.Prompt)})
}

func mcpSee(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Image    string `json:"image"`
		Question string `json:"question"`
		Model    string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Image == "" {
		return "", fmt.Errorf("see wants an image path")
	}
	if in.Question == "" {
		in.Question = "Describe this image."
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Vision)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Vision {
		return "", fmt.Errorf("%s cannot see — pick a vision model", spec.ID)
	}
	part, err := openai.ImagePart(in.Image)
	if err != nil {
		return "", err
	}
	msgs := []openai.Message{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": in.Question},
			part,
		},
	}}
	return mcpChat(ctx, spec, eng, msgs)
}

func mcpHear(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Audio    string `json:"audio"`
		Question string `json:"question"`
		Model    string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Audio == "" {
		return "", fmt.Errorf("hear wants an audio path")
	}
	if in.Question == "" {
		in.Question = "Transcribe what is said."
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Audio)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Audio {
		return "", fmt.Errorf("%s cannot hear — pick an audio model", spec.ID)
	}
	part, err := openai.AudioPart(in.Audio)
	if err != nil {
		return "", err
	}
	msgs := []openai.Message{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": in.Question},
			part,
		},
	}}
	return mcpChat(ctx, spec, eng, msgs)
}

func mcpEmbed(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Text == "" {
		return "", fmt.Errorf("embed wants text")
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Embed)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Embed {
		return "", fmt.Errorf("%s does not embed — pick an embed model", spec.ID)
	}
	var vec []float64
	err = modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		v, err := openai.Embed(ctx, url, key, spec.ID, in.Text)
		if err != nil {
			return err
		}
		vec = v
		return nil
	})
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(struct {
		Model      string    `json:"model"`
		Dimensions int       `json:"dimensions"`
		Embedding  []float64 `json:"embedding"`
	}{spec.ID, len(vec), vec})
	return string(out), nil
}

func mcpImagine(ctx context.Context, args json.RawMessage) (string, error) {
	return mcpSD(ctx, args, "imagine", catalog.Image, "png")
}

func mcpAnimate(ctx context.Context, args json.RawMessage) (string, error) {
	return mcpSD(ctx, args, "animate", catalog.Video, "webm")
}

func mcpSD(ctx context.Context, args json.RawMessage, verb string, kind catalog.Modality, ext string) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
		Image  string `json:"image"`
		Out    string `json:"out"`
		Model  string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("%s wants a prompt", verb)
	}
	spec, _, err := mcpResolve(ctx, in.Model, kind)
	if err != nil {
		return "", err
	}
	if spec.Runtime != catalog.SD || spec.Kind != kind {
		return "", fmt.Errorf("%s cannot %s — add a model with `fornax pull hf:… --kind %s`", spec.ID, verb, kind)
	}
	out, err := outPath(in.Out, verb, ext)
	if err != nil {
		return "", err
	}
	var extra []string
	if in.Image != "" {
		abs, err := filepath.Abs(in.Image)
		if err != nil {
			return "", err
		}
		extra = append(extra, "-i", abs)
	}
	if err := mcpMuteStdout(func() error {
		root, eng, err := modelrt.PrepareSD(ctx, spec)
		if err != nil {
			return err
		}
		return modelrt.RunSD(ctx, root, eng, spec, in.Prompt, out, -1, extra)
	}); err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	return "wrote " + out, nil
}

func mcpSay(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text  string `json:"text"`
		Out   string `json:"out"`
		Voice string `json:"voice"`
		Model string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Text == "" {
		return "", fmt.Errorf("say wants text")
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Speech)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Speech {
		return "", fmt.Errorf("%s does not speak — pick a speech model", spec.ID)
	}
	if in.Voice != "" {
		if info, err := os.Stat(in.Voice); err != nil || info.IsDir() {
			return "", fmt.Errorf("voice %s is not a readable audio file", in.Voice)
		}
	}
	if in.Out, err = outPath(in.Out, "say", "wav"); err != nil {
		return "", err
	}
	root, err := modelrt.PrepareSpeech(ctx, spec, eng)
	if err != nil {
		return "", err
	}
	if err := mcpMuteStdout(func() error {
		return modelrt.RunSay(ctx, root, eng, spec, in.Text, in.Out, in.Voice, "en", 0)
	}); err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(in.Out); err == nil {
		in.Out = abs
	}
	return "wrote " + in.Out, nil
}

// Typed questions to a decision model, answered with calibrated
// probabilities — the same /v1/systemone the `judge` command speaks. The
// reply is returned as JSON (answers, probabilities, legend, latency_ms).
func mcpJudge(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		State string `json:"state"`
		Ask   []struct {
			ID           string   `json:"id"`
			Type         string   `json:"type"`
			Instructions string   `json:"instructions"`
			Options      []string `json:"options"`
		} `json:"ask"`
		Model string `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.State) == "" {
		return "", fmt.Errorf("judge wants state — the document to ask about")
	}
	if len(in.Ask) == 0 {
		return "", fmt.Errorf("judge wants at least one ask")
	}
	questions := map[string]*openai.TypedQuestion{}
	for _, a := range in.Ask {
		if a.ID == "" {
			return "", fmt.Errorf("every ask needs an id")
		}
		switch a.Type {
		case "noul":
			if len(a.Options) > 0 {
				return "", fmt.Errorf("ask %s: noul takes no options", a.ID)
			}
		case "choice", "score":
			if len(a.Options) < 2 {
				return "", fmt.Errorf("ask %s: %s needs at least two options", a.ID, a.Type)
			}
		default:
			return "", fmt.Errorf("ask %s: type must be noul, choice or score", a.ID)
		}
		questions[a.ID] = &openai.TypedQuestion{Kind: a.Type, Instructions: a.Instructions, Options: a.Options}
	}
	spec, _, err := mcpResolve(ctx, in.Model, catalog.Decision)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Decision {
		return "", fmt.Errorf("%s chats, it does not judge — pull a kev or laya model", spec.ID)
	}
	var reply map[string]any
	err = modelrt.WithServer(ctx, spec, nil, func(url, key string) error {
		r, err := openai.SystemOne(ctx, url, key, modelrt.DecisionModel(spec), in.State, questions)
		if err != nil {
			return err
		}
		reply = r
		return nil
	})
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(reply)
	return string(out), nil
}

// Score documents against a query with a rerank model; one "score<TAB>doc"
// line per result, best first.
func mcpRerank(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      int      `json:"top_n"`
		Model     string   `json:"model"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("rerank wants a query")
	}
	if len(in.Documents) == 0 {
		return "", fmt.Errorf("rerank wants documents")
	}
	spec, eng, err := mcpResolve(ctx, in.Model, catalog.Rerank)
	if err != nil {
		return "", err
	}
	if spec.Kind != catalog.Rerank {
		return "", fmt.Errorf("%s does not rerank — pull a rerank model", spec.ID)
	}
	var lines strings.Builder
	err = modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		hits, err := openai.Rerank(ctx, url, key, spec.ID, in.Query, in.Documents, in.TopN)
		if err != nil {
			return err
		}
		for _, hit := range hits {
			fmt.Fprintf(&lines, "%.4f\t%s\n", hit.Score, hit.Text)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return lines.String(), nil
}

// outPath names a generated file. FORNAX_OUT_DIR, when set, holds the default
// name and any relative out, because a client may start this server in a
// folder it later replaces, like a plugin's install directory.
func outPath(out, verb, ext string) (string, error) {
	if out == "" {
		out = fmt.Sprintf("%s-%d.%s", verb, time.Now().Unix(), ext)
	}
	dir := os.Getenv("FORNAX_OUT_DIR")
	if dir == "" || filepath.IsAbs(out) {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, out), nil
}

func mcpList() (string, error) {
	root := paths.Home()
	var lines strings.Builder
	for _, spec := range modelrt.AllSpecs(root) {
		size := ui.HumanSize(spec.SizeBytes())
		if spec.Runtime == catalog.Apple {
			size = "os"
		}
		status := "-"
		if modelrt.Installed(root, spec) {
			status = "installed"
		}
		fmt.Fprintf(&lines, "%s %s %s %s\n", spec.ID, spec.Kind, size, status)
	}
	return lines.String(), nil
}

func mcpTools() []any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	obj := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	modelParam := func(kind string) map[string]any {
		return str(fmt.Sprintf("model id — default: the first installed %s model", kind))
	}
	return []any{
		map[string]any{
			"name":        "ask",
			"description": "One prompt, one reply from a local text or vision model.",
			"inputSchema": obj(map[string]any{
				"prompt": str("the prompt to send"),
				"model":  modelParam("text or vision"),
			}, []string{"prompt"}),
		},
		map[string]any{
			"name":        "see",
			"description": "Ask a vision model about an image file (png, jpg, webp, gif).",
			"inputSchema": obj(map[string]any{
				"image":    str("path to the image file"),
				"question": str("what to ask about it — default: describe the image"),
				"model":    modelParam("vision"),
			}, []string{"image"}),
		},
		map[string]any{
			"name":        "hear",
			"description": "Ask an audio model about a take — transcribes by default (wav, mp3, flac, m4a, ogg).",
			"inputSchema": obj(map[string]any{
				"audio":    str("path to the audio file"),
				"question": str("what to ask about it — default: transcribe what is said"),
				"model":    modelParam("audio"),
			}, []string{"audio"}),
		},
		map[string]any{
			"name":        "embed",
			"description": "Turn text into an embedding vector; returns JSON {model, dimensions, embedding}.",
			"inputSchema": obj(map[string]any{
				"text":  str("the text to embed"),
				"model": modelParam("embed"),
			}, []string{"text"}),
		},
		map[string]any{
			"name":        "imagine",
			"description": "Generate an image with a local stable-diffusion.cpp model.",
			"inputSchema": obj(map[string]any{
				"prompt": str("the image prompt"),
				"image":  str("path to a still image to start from"),
				"out":    str("PNG path — default: imagine-<timestamp>.png"),
				"model":  modelParam("image"),
			}, []string{"prompt"}),
		},
		map[string]any{
			"name":        "animate",
			"description": "Text or a still image to a short video clip (.webm) with the local video model. Takes minutes.",
			"inputSchema": obj(map[string]any{
				"prompt": str("what happens in the clip"),
				"image":  str("path to a still image the clip starts from"),
				"out":    str("output path, .webm .avi or .webp — default: animate-<timestamp>.webm"),
				"model":  modelParam("video"),
			}, []string{"prompt"}),
		},
		map[string]any{
			"name":        "say",
			"description": "Speak text to a WAV file with the speech model; voice clones a reference take.",
			"inputSchema": obj(map[string]any{
				"text":  str("the text to speak"),
				"out":   str("WAV path — default: say-<timestamp>.wav"),
				"voice": str("reference audio file to clone the voice from"),
				"model": modelParam("speech"),
			}, []string{"text"}),
		},
		map[string]any{
			"name":        "judge",
			"description": "Ask a local decision model (kev, laya) typed questions about a document and get calibrated probabilities — yes/no (noul), one of a set (choice), or a level on a scale (score). Returns JSON.",
			"inputSchema": obj(map[string]any{
				"state": str("the document the questions judge"),
				"ask": map[string]any{
					"type":        "array",
					"description": "the typed questions to ask",
					"items": obj(map[string]any{
						"id":           str("a short key this answer comes back under"),
						"type":         str("noul (yes/no), choice (one of a set) or score (a level on a scale)"),
						"instructions": str("what the question asks"),
						"options":      map[string]any{"type": "array", "items": str("an option label"), "description": "choice and score only, two or more, low to high for score"},
					}, []string{"id", "type", "instructions"}),
				},
				"model": modelParam("decision"),
			}, []string{"state", "ask"}),
		},
		map[string]any{
			"name":        "rerank",
			"description": "Score documents against a query with a local rerank model, best first. Returns one \"score<TAB>doc\" line per result.",
			"inputSchema": obj(map[string]any{
				"query":     str("what to rank the documents against"),
				"documents": map[string]any{"type": "array", "items": str("a document"), "description": "the documents to score"},
				"top_n":     map[string]any{"type": "integer", "description": "keep only the top N (default: all)"},
				"model":     modelParam("rerank"),
			}, []string{"query", "documents"}),
		},
		map[string]any{
			"name":        "models",
			"description": "Every model: id, kind, size, installed.",
			"inputSchema": obj(map[string]any{}, []string{}),
		},
	}
}
