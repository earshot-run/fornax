package studio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/earshot-run/fornax/internal/modelrt"
	"github.com/earshot-run/fornax/internal/paths"
)

// Records a text model in the test home as if `fornax pull` had installed it.
func installFakeChatModel(t *testing.T, root, id, kind string) {
	t.Helper()
	weights := []byte("GGUF fake weights")
	saved := filepath.Join(root, modelrt.CustomFile)
	var store struct {
		Version int              `json:"version"`
		Models  []map[string]any `json:"models"`
	}
	if raw, err := os.ReadFile(saved); err == nil {
		if err := json.Unmarshal(raw, &store); err != nil {
			t.Fatal(err)
		}
	}
	store.Version = 1
	store.Models = append(store.Models, map[string]any{
		"id": id, "kind": kind, "repo": "test/" + id, "revision": "0000000000000000000000000000000000000000",
		"file": id + ".gguf", "bytes": len(weights), "port": 7401 + len(store.Models),
	})
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saved, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := modelrt.Model(id)
	if spec == nil {
		t.Fatalf("%s did not register", id)
	}
	file := spec.Files()[0]
	if err := os.MkdirAll(filepath.Dir(paths.FilePath(root, spec, file)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.FilePath(root, spec, file), weights, 0o600); err != nil {
		t.Fatal(err)
	}
	if !modelrt.Installed(root, spec) {
		t.Fatalf("%s is not installed", id)
	}
}

// A stand-in for llama-server: /props, and a chat-completions handler the
// test provides. Counts how often the slot started it.
type fakeUpstream struct {
	srv     *httptest.Server
	starts  atomic.Int32
	mu      sync.Mutex
	bodies  []map[string]any
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeUpstream(t *testing.T, s *studio, handler func(w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/props":
			io.WriteString(w, `{"modalities":{"vision":true,"audio":false}}`)
		case "/v1/chat/completions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.bodies = append(f.bodies, body)
			f.mu.Unlock()
			f.handler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	s.chat.start = func(ctx context.Context, id string, ready func(url, key string)) error {
		f.starts.Add(1)
		ready(f.srv.URL+"/v1", "fake-key")
		<-ctx.Done()
		return nil
	}
	t.Cleanup(s.chat.stop)
	return f
}

func (f *fakeUpstream) lastBody(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("upstream saw no request")
	}
	return f.bodies[len(f.bodies)-1]
}

func streamChunks(chunks ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

func studioSend(t *testing.T, s *studio, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "127.0.0.1:7340"
	req.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	rec := httptest.NewRecorder()
	s.handler(7340).ServeHTTP(rec, req)
	return rec
}

type sseEvent struct{ name, data string }

func parseSSE(body string) []sseEvent {
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = name
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = data
			}
		}
		events = append(events, ev)
	}
	return events
}

func TestStudioChatStreamsThroughWarmSlot(t *testing.T) {
	s := testStudio(t)
	installFakeChatModel(t, s.root, "tiny-chat", "text")
	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"Let me think."}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"completion_tokens":2},"timings":{"predicted_per_second":42.5}}`,
	}
	up := newFakeUpstream(t, s, streamChunks(chunks...))

	body := `{"model":"tiny-chat","temperature":0.3,"max_tokens":64,"think":true,
		"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"},
		{"role":"assistant","content":"hey","reasoning":"not sent back"},{"role":"user","content":"again"}]}`
	rec := studioSend(t, s, "POST", "/api/chat", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/chat = %d %s", rec.Code, rec.Body)
	}
	events := parseSSE(rec.Body.String())
	if events[0].name != "status" || !strings.Contains(events[0].data, `"loading"`) {
		t.Errorf("first event = %+v, want a loading status", events[0])
	}
	if events[1].name != "status" || !strings.Contains(events[1].data, `"ready"`) || !strings.Contains(events[1].data, `"vision":true`) {
		t.Errorf("second event = %+v, want ready with the server's modalities", events[1])
	}
	var passed []string
	for _, ev := range events[2:] {
		passed = append(passed, ev.data)
	}
	if want := append(append([]string{}, chunks...), "[DONE]"); strings.Join(passed, "\n") != strings.Join(want, "\n") {
		t.Errorf("proxied chunks =\n%s\nwant\n%s", strings.Join(passed, "\n"), strings.Join(want, "\n"))
	}

	sent := up.lastBody(t)
	if sent["model"] != "tiny-chat" || sent["stream"] != true || sent["temperature"] != 0.3 || sent["max_tokens"] != float64(64) {
		t.Errorf("upstream body = %v", sent)
	}
	if _, ok := sent["top_p"]; ok {
		t.Errorf("unset top_p reached upstream: %v", sent)
	}
	if kwargs, _ := sent["chat_template_kwargs"].(map[string]any); kwargs["enable_thinking"] != true {
		t.Errorf("think did not reach the template: %v", sent["chat_template_kwargs"])
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("upstream messages = %v", msgs)
	}
	if raw, _ := json.Marshal(msgs[2]); strings.Contains(string(raw), "not sent back") {
		t.Errorf("reasoning went back upstream: %s", raw)
	}

	// Warm: the second message reuses the server and says nothing about loading.
	rec = studioSend(t, s, "POST", "/api/chat", `{"model":"tiny-chat","messages":[{"role":"user","content":"x"}]}`)
	if strings.Contains(rec.Body.String(), `"loading"`) {
		t.Errorf("warm slot reported loading again: %s", rec.Body)
	}
	if n := up.starts.Load(); n != 1 {
		t.Errorf("server started %d times, want 1", n)
	}
	if st := s.chat.status(); st.Model != "tiny-chat" || st.State != "ready" || st.Busy {
		t.Errorf("status = %+v, want tiny-chat ready and idle", st)
	}
}

func TestStudioChatRejectsBadRequests(t *testing.T) {
	s := testStudio(t)
	installFakeChatModel(t, s.root, "tiny-chat", "text")
	installFakeChatModel(t, s.root, "tiny-embed", "embed")
	up := newFakeUpstream(t, s, streamChunks())
	cases := map[string]string{
		"unknown model":     `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`,
		"not a chat model":  `{"model":"tiny-embed","messages":[{"role":"user","content":"hi"}]}`,
		"no user turn last": `{"model":"tiny-chat","messages":[{"role":"assistant","content":"hi"}]}`,
		"bad role":          `{"model":"tiny-chat","messages":[{"role":"tool","content":"x"},{"role":"user","content":"hi"}]}`,
		"missing ref":       `{"model":"tiny-chat","messages":[{"role":"user","content":"hi","refs":["0123456789abcdef.png"]}]}`,
		"traversal ref":     `{"model":"tiny-chat","messages":[{"role":"user","content":"hi","refs":["../../server.key"]}]}`,
	}
	for name, body := range cases {
		if rec := studioSend(t, s, "POST", "/api/chat", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if up.starts.Load() != 0 {
		t.Errorf("a rejected request started a server")
	}
	if rec := studioSend(t, s, "POST", "/api/chat/load", `{"model":"tiny-embed"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("load of an embed model: got %d", rec.Code)
	}
}

func TestStudioChatInlinesRefs(t *testing.T) {
	s := testStudio(t)
	installFakeChatModel(t, s.root, "tiny-omni", "audio")
	up := newFakeUpstream(t, s, streamChunks(`{"choices":[{"delta":{"content":"a cat"}}]}`))
	png := []byte("\x89PNG\r\n\x1a\n fake")
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt fake")
	for name, data := range map[string][]byte{"00000000000000aa.png": png, "00000000000000bb.wav": wav} {
		if err := os.WriteFile(filepath.Join(s.dir, "refs", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := studioSend(t, s, "POST", "/api/chat", `{"model":"tiny-omni","messages":[
		{"role":"user","content":"what is this?","refs":["00000000000000aa.png","00000000000000bb.wav"]}]}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("POST /api/chat = %d %s", rec.Code, rec.Body)
	}
	msgs := up.lastBody(t)["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v, want image, audio, text", parts)
	}
	img := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("image part = %.60s", img)
	}
	audio := parts[1].(map[string]any)["input_audio"].(map[string]any)
	if audio["format"] != "wav" || audio["data"] == "" {
		t.Errorf("audio part = %v", audio)
	}
	if text := parts[2].(map[string]any); text["type"] != "text" || text["text"] != "what is this?" {
		t.Errorf("text part = %v", text)
	}
}

func TestStudioChatSurfacesUpstreamErrors(t *testing.T) {
	cases := map[string]struct {
		handler func(w http.ResponseWriter, r *http.Request)
		want    string
	}{
		"refused": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"image input is not supported by this model"}}`)
		}, "image input is not supported by this model"},
		"error chunk": {streamChunks(`{"choices":[{"delta":{"content":"a"}}]}`, `{"error":{"message":"context size exceeded"}}`), "context size exceeded"},
		"cut off": {func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		}, "stopped before the reply finished"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := testStudio(t)
			installFakeChatModel(t, s.root, "tiny-chat", "text")
			newFakeUpstream(t, s, tc.handler)
			rec := studioSend(t, s, "POST", "/api/chat", `{"model":"tiny-chat","messages":[{"role":"user","content":"hi"}]}`)
			events := parseSSE(rec.Body.String())
			last := events[len(events)-1]
			if last.name != "error" || !strings.Contains(last.data, tc.want) {
				t.Errorf("last event = %+v, want an error carrying %q", last, tc.want)
			}
		})
	}
}

func TestStudioChatDisconnectCancelsUpstream(t *testing.T) {
	s := testStudio(t)
	installFakeChatModel(t, s.root, "tiny-chat", "text")
	gone := make(chan struct{})
	newFakeUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	srv := httptest.NewUnstartedServer(s.handler(port))
	srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/chat", strings.NewReader(`{"model":"tiny-chat","messages":[{"role":"user","content":"hi"}]}`))
	req.AddCookie(&http.Cookie{Name: studioCookie, Value: s.key})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before the first token: %v", err)
		}
		if strings.Contains(line, `"tok"`) {
			break
		}
	}
	cancel()
	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the page's request did not cancel the upstream one")
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.chat.status().Busy && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.chat.status().Busy {
		t.Error("the slot still counts the cancelled request as busy")
	}
}

func TestChatSlotSwitchesAndReaps(t *testing.T) {
	slot := newChatSlot()
	var mu sync.Mutex
	running := map[string]bool{}
	overlap := false
	slot.start = func(ctx context.Context, id string, ready func(url, key string)) error {
		mu.Lock()
		if len(running) > 0 {
			overlap = true
		}
		running[id] = true
		mu.Unlock()
		ready("http://127.0.0.1:1/v1", "k")
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // reaping takes a moment
		mu.Lock()
		delete(running, id)
		mu.Unlock()
		return nil
	}
	ctx := context.Background()

	// Two callers for the same model share one load.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, release, err := slot.acquire(ctx, "a")
			if err != nil {
				t.Error(err)
				return
			}
			release()
		}()
	}
	wg.Wait()
	_, _, release, err := slot.acquire(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	release()
	mu.Lock()
	if overlap {
		t.Error("b started before a was reaped")
	}
	if !running["b"] || running["a"] {
		t.Errorf("running = %v, want only b", running)
	}
	mu.Unlock()

	slot.reapIdle()
	if slot.status().Model != "b" {
		t.Error("reaped a model that was used just now")
	}
	slot.idle = 0
	slot.reapIdle()
	if st := slot.status(); st.Model != "" {
		t.Errorf("status after reaping = %+v, want empty", st)
	}
	mu.Lock()
	if len(running) != 0 {
		t.Errorf("still running after reap: %v", running)
	}
	mu.Unlock()
}

func TestChatSlotReportsFailedLoad(t *testing.T) {
	slot := newChatSlot()
	var calls atomic.Int32
	slot.start = func(ctx context.Context, id string, ready func(url, key string)) error {
		calls.Add(1)
		return fmt.Errorf("the model server exited before it was ready")
	}
	for range 2 {
		if _, _, _, err := slot.acquire(context.Background(), "a"); err == nil || !strings.Contains(err.Error(), "exited") {
			t.Fatalf("acquire err = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("a failed load was not retried: %d starts", calls.Load())
	}
}

func TestStudioChatConversations(t *testing.T) {
	s := testStudio(t)
	older := `{"title":"  first   chat ","model":"m","messages":[{"role":"user","content":"hi","refs":["00000000000000aa.png"]},{"role":"assistant","content":"hello","reasoning":"hmm","tokens":3,"perSecond":40}]}`
	if rec := studioSend(t, s, "PUT", "/api/chats/00000000000000c1", older); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	time.Sleep(5 * time.Millisecond)
	if rec := studioSend(t, s, "PUT", "/api/chats/00000000000000c2", `{"title":"second","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}

	var list []chatSummary
	json.Unmarshal(studioSend(t, s, "GET", "/api/chats", "").Body.Bytes(), &list)
	if len(list) != 2 || list[0].ID != "00000000000000c2" || list[1].Title != "first chat" || list[1].Count != 2 {
		t.Fatalf("list = %+v, want newest first with a tidied title", list)
	}

	var conv chatConversation
	json.Unmarshal(studioSend(t, s, "GET", "/api/chats/00000000000000c1", "").Body.Bytes(), &conv)
	if len(conv.Messages) != 2 || conv.Messages[0].Refs[0] != "00000000000000aa.png" || conv.Messages[1].Reasoning != "hmm" || conv.Messages[1].PerSecond != 40 {
		t.Errorf("conversation did not round-trip: %+v", conv)
	}

	if rec := studioSend(t, s, "PATCH", "/api/chats/00000000000000c1", `{"title":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d", rec.Code)
	}
	json.Unmarshal(studioSend(t, s, "GET", "/api/chats", "").Body.Bytes(), &list)
	if list[1].Title != "renamed" {
		t.Errorf("rename moved or missed: %+v", list)
	}

	if rec := studioSend(t, s, "PUT", "/api/chats/00000000000000c3", `{"messages":[{"role":"user","content":"x","refs":["../../server.key"]}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT with a path in refs: got %d, want 400", rec.Code)
	}
	for _, target := range []string{"/api/chats/..%2fx", "/api/chats/nothex"} {
		if rec := studioSend(t, s, "GET", target, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", target, rec.Code)
		}
	}

	if rec := studioSend(t, s, "DELETE", "/api/chats/00000000000000c1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if rec := studioSend(t, s, "GET", "/api/chats/00000000000000c1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleted chat still answers: %d", rec.Code)
	}
}

// Two shutdown paths call stop at once (the reap goroutine and Serve);
// neither may return while a server is still being reaped.
func TestChatSlotStopWaitsForReap(t *testing.T) {
	slot := newChatSlot()
	var reaped atomic.Bool
	slot.start = func(ctx context.Context, id string, ready func(url, key string)) error {
		ready("http://127.0.0.1:1/v1", "k")
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		reaped.Store(true)
		return nil
	}
	_, _, release, err := slot.acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot.stop()
			if !reaped.Load() {
				t.Error("stop returned before the server was reaped")
			}
		}()
	}
	wg.Wait()
	if _, _, _, err := slot.acquire(context.Background(), "a"); err == nil {
		t.Error("a stopped slot started a new server")
	}
}

func TestChatSearchIncludesContentAndPreservesOrder(t *testing.T) {
	s := testStudio(t)
	for _, item := range []struct{ id, body string }{
		{"00000000000000c1", `{"title":"First chat","model":"local-qwen","messages":[{"role":"user","content":"The café is open"}]}`},
		{"00000000000000c2", `{"title":"Second chat","messages":[{"role":"assistant","content":"Answer","reasoning":"Consider the CAFÉ"}]}`},
		{"00000000000000c3", `{"title":"Unrelated","messages":[{"role":"user","content":"Hello"}]}`},
	} {
		if rec := studioSend(t, s, "PUT", "/api/chats/"+item.id, item.body); rec.Code != http.StatusOK {
			t.Fatalf("save %s = %d", item.id, rec.Code)
		}
	}
	for _, query := range []struct {
		value string
		want  []string
	}{
		{"CAF%C3%89", []string{"00000000000000c2", "00000000000000c1"}},
		{"%20LOCAL-QWEN%20", []string{"00000000000000c1"}},
		{"second", []string{"00000000000000c2"}},
		{"missing", nil},
	} {
		rec := studioSend(t, s, "GET", "/api/chats?q="+query.value, "")
		var list []chatSummary
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
			t.Fatalf("query %s: %d %s", query.value, rec.Code, rec.Body.String())
		}
		if len(list) != len(query.want) {
			t.Fatalf("query %s: %+v, want %v", query.value, list, query.want)
		}
		for i := range list {
			if list[i].ID != query.want[i] {
				t.Errorf("query %s: %+v, want %v", query.value, list, query.want)
			}
		}
		if strings.Contains(rec.Body.String(), "Consider") {
			t.Error("search returned message content instead of summaries")
		}
	}
}
