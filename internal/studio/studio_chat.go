package studio

// Studio's chat: conversations with text, vision and audio models over a
// warm model server. One model stays loaded between messages — a reply
// should not pay for a cold start — and it is let go after chatIdle
// without use, or the moment the page picks a different model.
//
// The server itself comes from modelrt.WithServer, the same pull → verify →
// spawn → reap path every command takes; the slot only holds its callback
// open. Conversations are JSON files under studio/chats. Attachments stay
// ref names there and are inlined into the request on the way upstream.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
)

const (
	chatIdle      = 10 * time.Minute
	chatMaxBody   = 8 << 20
	chatMaxTitle  = 120
	chatReapEvery = 30 * time.Second
)

// What a loaded server can take besides text, from llama-server's /props.
type chatModalities struct {
	Vision bool `json:"vision"`
	Audio  bool `json:"audio"`
}

// One model's server, from the moment it was asked for until it is reaped.
type chatLoad struct {
	id     string
	cancel context.CancelFunc
	ready  chan struct{} // closed once url/key or err is set
	done   chan struct{} // closed once the server is gone

	url, key   string
	modalities *chatModalities
	err        error
	active     int
	used       time.Time
}

type chatSlot struct {
	// Serves model id until ctx ends, calling ready once it answers. Tests
	// swap it for a fake server.
	start func(ctx context.Context, id string, ready func(url, key string)) error
	idle  time.Duration
	// Runs before a model starts loading, to free the GPU for it.
	beforeLoad func()

	mu     sync.Mutex
	cur    *chatLoad
	closed bool
	// Every server not yet reaped, so stop can outwait all of them: the
	// studio exiting first would orphan one.
	running sync.WaitGroup
}

func newChatSlot() *chatSlot {
	return &chatSlot{start: serveChatModel, idle: chatIdle}
}

func serveChatModel(ctx context.Context, id string, ready func(url, key string)) error {
	spec, eng, err := modelrt.Resolve(ctx, id)
	if err != nil {
		return err
	}
	return modelrt.WithServer(ctx, spec, eng, func(url, key string) error {
		ready(url, key)
		<-ctx.Done()
		return nil
	})
}

// Starts id (stopping whatever else is loaded) or joins the load already
// under way, and waits until it answers or ctx ends. release must be
// called once the caller is done talking to it.
func (c *chatSlot) acquire(ctx context.Context, id string) (url, key string, release func(), err error) {
	load := c.load(id)
	select {
	case <-ctx.Done():
		c.release(load)
		return "", "", nil, ctx.Err()
	case <-load.ready:
	}
	if load.err != nil {
		c.release(load)
		return "", "", nil, load.err
	}
	return load.url, load.key, func() { c.release(load) }, nil
}

// The load for id, begun if needed, with one more user counted on it.
func (c *chatSlot) load(id string) *chatLoad {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && c.cur.id == id && c.cur.err == nil {
		c.cur.active++
		c.cur.used = time.Now()
		return c.cur
	}
	previous := c.cur
	ctx, cancel := context.WithCancel(context.Background())
	load := &chatLoad{id: id, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), active: 1, used: time.Now()}
	if c.closed {
		cancel()
		load.err = errors.New("studio is shutting down")
		close(load.ready)
		close(load.done)
		return load
	}
	c.cur = load
	c.running.Add(1)
	go c.run(ctx, load, previous)
	return load
}

func (c *chatSlot) run(ctx context.Context, load *chatLoad, previous *chatLoad) {
	defer c.running.Done()
	defer close(load.done)
	// Two models at once would fight over the same GPU memory.
	if previous != nil {
		previous.cancel()
		<-previous.done
	}
	if c.beforeLoad != nil {
		c.beforeLoad()
	}
	var once sync.Once
	err := c.start(ctx, load.id, func(url, key string) {
		modalities := probeModalities(url, key)
		once.Do(func() {
			c.mu.Lock()
			load.url, load.key, load.modalities = url, key, modalities
			c.mu.Unlock()
			close(load.ready)
		})
	})
	switch {
	case ctx.Err() != nil:
		err = errors.New("the model was unloaded")
	case err == nil:
		err = errors.New("the model server stopped")
	}
	once.Do(func() {
		c.mu.Lock()
		load.err = err
		c.mu.Unlock()
		close(load.ready)
	})
	c.mu.Lock()
	if c.cur == load {
		c.cur = nil
	}
	c.mu.Unlock()
}

func (c *chatSlot) release(load *chatLoad) {
	c.mu.Lock()
	defer c.mu.Unlock()
	load.active--
	load.used = time.Now()
}

func probeModalities(url, key string) *chatModalities {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	props := openai.GetJSON(client, strings.TrimSuffix(url, "/v1")+"/props", key)
	raw, ok := props["modalities"].(map[string]any)
	if !ok {
		return nil
	}
	vision, _ := raw["vision"].(bool)
	audio, _ := raw["audio"].(bool)
	return &chatModalities{Vision: vision, Audio: audio}
}

// Unloads id if it is the one loaded ("" unloads whatever is), and waits
// for its server to be gone.
func (c *chatSlot) unload(id string) {
	c.mu.Lock()
	load := c.cur
	if load == nil || (id != "" && load.id != id) {
		c.mu.Unlock()
		return
	}
	c.cur = nil
	c.mu.Unlock()
	load.cancel()
	<-load.done
}

func (c *chatSlot) reap(ctx context.Context) {
	tick := time.NewTicker(chatReapEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.stop()
			return
		case <-tick.C:
			c.reapIdle()
		}
	}
}

func (c *chatSlot) reapIdle() {
	c.mu.Lock()
	load := c.cur
	if load == nil || load.active > 0 || time.Since(load.used) < c.idle {
		c.mu.Unlock()
		return
	}
	c.cur = nil
	c.mu.Unlock()
	load.cancel()
	<-load.done
}

// Unloads for good and returns once every server the slot started is gone.
func (c *chatSlot) stop() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.unload("")
	c.running.Wait()
}

func (c *chatSlot) unloadAll() { c.unload("") }

type chatStatus struct {
	Model      string          `json:"model,omitempty"`
	State      string          `json:"state"` // "", loading, ready
	Modalities *chatModalities `json:"modalities,omitempty"`
	Busy       bool            `json:"busy,omitempty"`
}

func (c *chatSlot) status() chatStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	load := c.cur
	if load == nil {
		return chatStatus{}
	}
	state := "loading"
	select {
	case <-load.ready:
		if load.err != nil {
			return chatStatus{}
		}
		state = "ready"
	default:
	}
	return chatStatus{Model: load.id, State: state, Modalities: load.modalities, Busy: load.active > 0}
}

func (s *studio) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("GET /api/chat/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.chat.status())
	})
	mux.HandleFunc("POST /api/chat/load", s.handleChatLoad)
	mux.HandleFunc("POST /api/chat/unload", func(w http.ResponseWriter, _ *http.Request) {
		s.chat.unloadAll()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/chats", s.handleListChats)
	mux.HandleFunc("GET /api/chats/{id}", s.handleGetChat)
	mux.HandleFunc("PUT /api/chats/{id}", s.handlePutChat)
	mux.HandleFunc("PATCH /api/chats/{id}", s.handleRenameChat)
	mux.HandleFunc("DELETE /api/chats/{id}", s.handleDeleteChat)
}

// A chat model the page may use: installed, and one that converses.
func chatModel(root, id string) (*catalog.Spec, error) {
	spec := modelrt.Model(id)
	if spec == nil {
		return nil, fmt.Errorf("%q is not one of your models", id)
	}
	if !modelrt.Installed(root, spec) {
		return nil, fmt.Errorf("%s is not installed — `fornax pull %s`", spec.ID, spec.ID)
	}
	if spec.Kind == catalog.Decision {
		return nil, fmt.Errorf("%s answers typed questions, it does not chat", spec.ID)
	}
	if err := modelrt.RequireChat(spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// Warms a model up ahead of the first message, so picking one in the page
// starts loading it.
func (s *studio) handleChatLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	spec, err := chatModel(s.root, req.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.chat.release(s.chat.load(spec.ID))
	writeJSON(w, http.StatusOK, s.chat.status())
}

// A message as the page and the saved conversation hold it. Refs name
// uploaded files (studio/refs); Reasoning and the stats are the reply's
// thinking and speed, kept for display and never sent back upstream.
type chatMessage struct {
	Role      string   `json:"role"`
	Content   string   `json:"content"`
	Refs      []string `json:"refs,omitempty"`
	Reasoning string   `json:"reasoning,omitempty"`
	Model     string   `json:"model,omitempty"`
	Tokens    int      `json:"tokens,omitempty"`
	PerSecond float64  `json:"perSecond,omitempty"`
	Thought   float64  `json:"thought,omitempty"`
	Stopped   bool     `json:"stopped,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Sampling; nil leaves the server's default.
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"top_p"`
	TopK          *int     `json:"top_k"`
	MinP          *float64 `json:"min_p"`
	RepeatPenalty *float64 `json:"repeat_penalty"`
	MaxTokens     *int     `json:"max_tokens"`
	Seed          *int64   `json:"seed"`
	// Qwen3-style thinking, through the chat template; nil leaves it as served.
	Think *bool `json:"think"`
}

// The upstream request body: OpenAI chat completions with the refs inlined
// as image_url data URLs and input_audio parts.
func (s *studio) upstreamChat(req *chatRequest, id string) (map[string]any, error) {
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if !slices.Contains([]string{"system", "user", "assistant"}, m.Role) {
			return nil, fmt.Errorf("unknown message role %q", m.Role)
		}
		if len(m.Refs) == 0 || m.Role != "user" {
			messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
			continue
		}
		var parts []map[string]any
		for _, ref := range m.Refs {
			path := s.refPath(ref)
			if path == "" {
				return nil, fmt.Errorf("attachment %q is gone — attach it again", ref)
			}
			var part map[string]any
			var err error
			switch filepath.Ext(path) {
			case ".png", ".jpg", ".webp":
				part, err = openai.ImagePart(path)
			case ".wav", ".mp3", ".ogg":
				part, err = openai.AudioPart(path)
			default:
				err = fmt.Errorf("attachment %q is not an image or audio clip", ref)
			}
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		if m.Content != "" {
			parts = append(parts, map[string]any{"type": "text", "text": m.Content})
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": parts})
	}
	body := map[string]any{
		"model":          id,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	for key, value := range map[string]any{
		"temperature": req.Temperature, "top_p": req.TopP, "top_k": req.TopK, "min_p": req.MinP,
		"repeat_penalty": req.RepeatPenalty, "max_tokens": req.MaxTokens, "seed": req.Seed,
	} {
		switch v := value.(type) {
		case *float64:
			if v != nil {
				body[key] = *v
			}
		case *int:
			if v != nil {
				body[key] = *v
			}
		case *int64:
			if v != nil {
				body[key] = *v
			}
		}
	}
	if req.Think != nil {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": *req.Think}
		if *req.Think {
			body["reasoning_format"] = "deepseek"
		}
	}
	return body, nil
}

// POST /api/chat streams server-sent events: `status` while the model
// loads, then the upstream chat-completions chunks verbatim (content,
// reasoning_content, the final usage/timings), then [DONE] — or one
// `error` event, readable as it stands.
func (s *studio) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, chatMaxBody)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	spec, err := chatModel(s.root, req.Model)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" {
		http.Error(w, "the conversation must end with a user message", http.StatusBadRequest)
		return
	}
	body, err := s.upstreamChat(&req, spec.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, value any) {
		raw, _ := json.Marshal(value)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		flusher.Flush()
	}
	fail := func(err error) {
		if r.Context().Err() == nil {
			send("error", map[string]string{"message": err.Error()})
		}
	}

	if st := s.chat.status(); st.Model != spec.ID || st.State != "ready" {
		send("status", chatStatus{Model: spec.ID, State: "loading"})
	}
	url, key, release, err := s.chat.acquire(r.Context(), spec.ID)
	if err != nil {
		fail(fmt.Errorf("could not load %s: %w", spec.ID, err))
		return
	}
	defer release()
	send("status", s.chat.status())

	raw, _ := json.Marshal(body)
	resp, err := openai.Post(r.Context(), url+"/chat/completions", key, raw)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(openai.Error(resp))
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), openai.MaxBody)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Error != nil {
			fail(fmt.Errorf("the model server failed: %s", chunk.Error.Message))
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		fail(fmt.Errorf("the reply stream broke: %w", err))
		return
	}
	fail(errors.New("the model server stopped before the reply finished"))
}

// ---------- conversations ----------

type chatConversation struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Model    string        `json:"model,omitempty"`
	Created  time.Time     `json:"created"`
	Updated  time.Time     `json:"updated"`
	Messages []chatMessage `json:"messages"`
}

type chatSummary struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Model   string    `json:"model,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Count   int       `json:"count"`
}

func (s *studio) chatPath(id string) string {
	return filepath.Join(s.dir, "chats", id+".json")
}

func (s *studio) readChat(id string) (*chatConversation, error) {
	if !studioIDPattern.MatchString(id) {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(s.chatPath(id))
	if err != nil {
		return nil, err
	}
	var conv chatConversation
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, err
	}
	conv.ID = id
	return &conv, nil
}

func (s *studio) writeChat(conv *chatConversation) error {
	raw, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicPrivate(s.chatPath(conv.ID), raw)
}

// Newest first by last change.
func (s *studio) handleListChats(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "chats"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list := []chatSummary{}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		conv, err := s.readChat(id)
		if err != nil {
			continue
		}
		list = append(list, chatSummary{ID: conv.ID, Title: conv.Title, Model: conv.Model, Created: conv.Created, Updated: conv.Updated, Count: len(conv.Messages)})
	}
	slices.SortFunc(list, func(a, b chatSummary) int { return b.Updated.Compare(a.Updated) })
	writeJSON(w, http.StatusOK, list)
}

func (s *studio) handleGetChat(w http.ResponseWriter, r *http.Request) {
	conv, err := s.readChat(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

// Saves the whole conversation; the page owns its content.
func (s *studio) handlePutChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !studioIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var conv chatConversation
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, chatMaxBody)).Decode(&conv); err != nil {
		http.Error(w, "bad conversation body", http.StatusBadRequest)
		return
	}
	for _, m := range conv.Messages {
		if !slices.Contains([]string{"system", "user", "assistant"}, m.Role) {
			http.Error(w, fmt.Sprintf("unknown message role %q", m.Role), http.StatusBadRequest)
			return
		}
		for _, ref := range m.Refs {
			if !studioFilePattern.MatchString(ref) {
				http.Error(w, fmt.Sprintf("attachment %q is not a studio file", ref), http.StatusBadRequest)
				return
			}
		}
	}
	conv.ID = id
	conv.Title = cleanChatTitle(conv.Title)
	if old, err := s.readChat(id); err == nil {
		conv.Created = old.Created
	} else if conv.Created.IsZero() {
		conv.Created = time.Now().UTC()
	}
	conv.Updated = time.Now().UTC()
	if conv.Messages == nil {
		conv.Messages = []chatMessage{}
	}
	if err := s.writeChat(&conv); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

// Renames without touching the order: a new title is not new conversation.
func (s *studio) handleRenameChat(w http.ResponseWriter, r *http.Request) {
	conv, err := s.readChat(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	conv.Title = cleanChatTitle(req.Title)
	if err := s.writeChat(conv); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *studio) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !studioIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(s.chatPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func cleanChatTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if runes := []rune(title); len(runes) > chatMaxTitle {
		title = string(runes[:chatMaxTitle])
	}
	return title
}
