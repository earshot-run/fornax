package main

// `mcp` — a newline-delimited JSON-RPC 2.0 MCP server on stdin/stdout, so
// MCP-aware agents (Claude Code, Cursor) can drive the local models. stdout
// carries only protocol replies: every spinner, progress bar and engine
// line stays on stderr, and the two foreground helpers that would print a
// success line (runSay, runDraw) get os.Stdout redirected while they run.
// Requests run one at a time — a model load can take minutes and the
// scratch servers are spawned --parallel 1 anyway.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The MCP revision this server speaks; serverInfo.version rides on the
// build stamp in version.go.
const mcpProtocol = "2024-11-05"

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

func cmdMCP(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax mcp   (serves MCP on stdin/stdout)")
	}
	out := bufio.NewWriter(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*maxHTTPBody)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
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
		reply := mcpDispatch(ctx, &req)
		reply.ID = req.ID
		writeMCP(out, reply)
	}
	return scanner.Err()
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

func mcpDispatch(ctx context.Context, req *mcpRequest) mcpReply {
	reply := mcpReply{JSONRPC: "2.0"}
	result, rpcErr := mcpHandle(ctx, req.Method, req.Params)
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

func mcpHandle(ctx context.Context, method string, params json.RawMessage) (any, *mcpError) {
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
	case "draw":
		return mcpDraw(ctx, args)
	case "say":
		return mcpSay(ctx, args)
	case "list":
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

// The model to serve: the explicit id, or the first catalog spec of a
// wanted kind — an installed one when there is a choice.
func mcpResolve(id string, kinds ...modality) (*modelSpec, *engineSpec, error) {
	if id == "" {
		spec := mcpDefaultSpec(home(), kinds...)
		if spec == nil {
			return nil, nil, fmt.Errorf("the catalog has no %s model", kinds[0])
		}
		id = spec.id
	}
	return resolve(id)
}

func mcpDefaultSpec(root string, kinds ...modality) *modelSpec {
	var fallback *modelSpec
	for _, spec := range allSpecs(root) {
		match := false
		for _, kind := range kinds {
			if spec.kind == kind {
				match = true
			}
		}
		if !match {
			continue
		}
		if modelInstalled(root, spec) {
			return spec
		}
		if fallback == nil {
			fallback = spec
		}
	}
	return fallback
}

// runAsk/runSee/runHear all stream to stdout; the MCP path is the same
// withServer + chat completion minus the printing.
func mcpChat(ctx context.Context, spec *modelSpec, eng *engineSpec, msgs []message) (string, error) {
	var reply *chatReply
	err := withServer(ctx, spec, eng, func(url, key string) error {
		r, err := chatOnce(ctx, url, key, spec.id, msgs, -1)
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

// runSay/runDraw end with a `wrote …` line on stdout. Under MCP that fd is
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
	spec, eng, err := mcpResolve(in.Model, modalText, modalVision, modalAudio)
	if err != nil {
		return "", err
	}
	switch {
	case spec.rt == runtimeKev:
		return "", fmt.Errorf("%s answers typed questions, not prompts", spec.id)
	case spec.kind == modalEmbed:
		return "", fmt.Errorf("%s embeds, it does not chat — use the embed tool", spec.id)
	case spec.kind == modalImage:
		return "", fmt.Errorf("%s draws, it does not chat — use the draw tool", spec.id)
	case spec.kind == modalSpeech:
		return "", fmt.Errorf("%s speaks, it does not chat — use the say tool", spec.id)
	case spec.kind == modalRerank:
		return "", fmt.Errorf("%s reranks, it does not chat", spec.id)
	}
	return mcpChat(ctx, spec, eng, []message{textMessage("user", in.Prompt)})
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
	spec, eng, err := mcpResolve(in.Model, modalVision)
	if err != nil {
		return "", err
	}
	if spec.kind != modalVision {
		return "", fmt.Errorf("%s cannot see — pick a vision model", spec.id)
	}
	part, err := imagePart(in.Image)
	if err != nil {
		return "", err
	}
	msgs := []message{{
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
	spec, eng, err := mcpResolve(in.Model, modalAudio)
	if err != nil {
		return "", err
	}
	if spec.kind != modalAudio {
		return "", fmt.Errorf("%s cannot hear — pick an audio model", spec.id)
	}
	part, err := audioPart(in.Audio)
	if err != nil {
		return "", err
	}
	msgs := []message{{
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
	spec, eng, err := mcpResolve(in.Model, modalEmbed)
	if err != nil {
		return "", err
	}
	if spec.kind != modalEmbed {
		return "", fmt.Errorf("%s does not embed — pick an embed model", spec.id)
	}
	var vec []float64
	err = withServer(ctx, spec, eng, func(url, key string) error {
		v, err := embedOnce(ctx, url, key, spec.id, in.Text)
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
	}{spec.id, len(vec), vec})
	return string(out), nil
}

func mcpDraw(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
		Out    string `json:"out"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("draw wants a prompt")
	}
	spec, eng, err := mcpResolve("", modalImage)
	if err != nil {
		return "", err
	}
	if spec.rt != runtimeSD {
		return "", fmt.Errorf("%s does not draw — pick an image model", spec.id)
	}
	if in.Out == "" {
		in.Out = fmt.Sprintf("draw-%d.png", time.Now().Unix())
	}
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return "", err
	}
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return "", err
	}
	verifying.stop("")
	if err := mcpMuteStdout(func() error {
		return runDraw(ctx, root, eng, spec, in.Prompt, "", in.Out, 512, 512, 4, -1)
	}); err != nil {
		return "", err
	}
	return "wrote " + in.Out, nil
}

func mcpSay(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text  string `json:"text"`
		Out   string `json:"out"`
		Voice string `json:"voice"`
	}
	if err := mcpArgs(args, &in); err != nil {
		return "", err
	}
	if in.Text == "" {
		return "", fmt.Errorf("say wants text")
	}
	spec, eng, err := mcpResolve("", modalSpeech)
	if err != nil {
		return "", err
	}
	if spec.kind != modalSpeech {
		return "", fmt.Errorf("%s does not speak — pick a speech model", spec.id)
	}
	if in.Voice != "" {
		if info, err := os.Stat(in.Voice); err != nil || info.IsDir() {
			return "", fmt.Errorf("voice %s is not a readable audio file", in.Voice)
		}
	}
	if in.Out == "" {
		in.Out = fmt.Sprintf("say-%d.wav", time.Now().Unix())
	}
	root := home()
	if err := pull(ctx, spec, eng); err != nil {
		return "", err
	}
	verifying := spin("verifying " + spec.id)
	if err := rehash(root, spec); err != nil {
		verifying.stop("")
		return "", err
	}
	verifying.stop("")
	if err := mcpMuteStdout(func() error {
		return runSay(ctx, root, eng, spec, in.Text, in.Out, in.Voice, "en", 0)
	}); err != nil {
		return "", err
	}
	return "wrote " + in.Out, nil
}

func mcpList() (string, error) {
	root := home()
	var lines strings.Builder
	for _, spec := range allSpecs(root) {
		size := humanSize(spec.sizeBytes())
		if spec.rt == runtimeApple {
			size = "os"
		}
		status := "-"
		if modelInstalled(root, spec) {
			status = "installed"
		}
		fmt.Fprintf(&lines, "%s %s %s %s\n", spec.id, spec.kind, size, status)
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
	model := func(kind string) map[string]any {
		return str(fmt.Sprintf("catalog model id — default: the first installed %s model", kind))
	}
	return []any{
		map[string]any{
			"name":        "ask",
			"description": "One prompt, one reply from a local text or vision model.",
			"inputSchema": obj(map[string]any{
				"prompt": str("the prompt to send"),
				"model":  model("text or vision"),
			}, []string{"prompt"}),
		},
		map[string]any{
			"name":        "see",
			"description": "Ask a vision model about an image file (png, jpg, webp, gif).",
			"inputSchema": obj(map[string]any{
				"image":    str("path to the image file"),
				"question": str("what to ask about it — default: describe the image"),
				"model":    model("vision"),
			}, []string{"image"}),
		},
		map[string]any{
			"name":        "hear",
			"description": "Ask an audio model about a take — transcribes by default (wav, mp3, flac, m4a, ogg).",
			"inputSchema": obj(map[string]any{
				"audio":    str("path to the audio file"),
				"question": str("what to ask about it — default: transcribe what is said"),
				"model":    model("audio"),
			}, []string{"audio"}),
		},
		map[string]any{
			"name":        "embed",
			"description": "Turn text into an embedding vector; returns JSON {model, dimensions, embedding}.",
			"inputSchema": obj(map[string]any{
				"text":  str("the text to embed"),
				"model": model("embed"),
			}, []string{"text"}),
		},
		map[string]any{
			"name":        "draw",
			"description": "Generate a 512x512 PNG with the stable-diffusion.cpp image model.",
			"inputSchema": obj(map[string]any{
				"prompt": str("the image prompt"),
				"out":    str("PNG path — default: draw-<timestamp>.png"),
			}, []string{"prompt"}),
		},
		map[string]any{
			"name":        "say",
			"description": "Speak text to a WAV file with the speech model; voice clones a reference take.",
			"inputSchema": obj(map[string]any{
				"text":  str("the text to speak"),
				"out":   str("WAV path — default: say-<timestamp>.wav"),
				"voice": str("reference audio file to clone the voice from"),
			}, []string{"text"}),
		},
		map[string]any{
			"name":        "list",
			"description": "Every catalog model: id, kind, size, installed.",
			"inputSchema": obj(map[string]any{}, []string{}),
		},
	}
}
