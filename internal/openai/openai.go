package openai

// Package openai talks to the running model server: one chat-completions
// client that both the one-shot commands and the interactive REPL share, the
// embeddings call, the keyed GET behind readiness and listing probes, and
// the raw POST the rerank and systemone clients build on. Content is text by
// default and gains image/audio parts for vision and audio models.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxBody caps what fornax will read from any model server or HTTP API.
const MaxBody = 2 * 1024 * 1024

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func TextMessage(role, text string) Message {
	return Message{Role: role, Content: text}
}

type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	PredictedPerSec  float64 `json:"predicted_per_second"`
}

type Reply struct {
	Text    string
	Usage   *Usage
	Timings map[string]any
}

// One non-streaming completion. `content` is a plain string or a parts array.
func Once(ctx context.Context, url, key, model string, messages []Message, maxTokens int) (*Reply, error) {
	return OnceFull(ctx, url, key, model, messages, maxTokens, nil)
}

// Once with extra request-body fields merged in — response_format and
// friends for structured output. extra keys override the defaults.
func OnceFull(ctx context.Context, url, key, model string, messages []Message, maxTokens int, extra map[string]any) (*Reply, error) {
	req := map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     false,
	}
	for k, v := range extra {
		req[k] = v
	}
	body, _ := json.Marshal(req)
	resp, err := Post(ctx, url+"/chat/completions", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Error(resp)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage   *Usage         `json:"usage"`
		Timings map[string]any `json:"timings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("the server returned no reply")
	}
	return &Reply{
		Text:    parsed.Choices[0].Message.Content,
		Usage:   parsed.Usage,
		Timings: parsed.Timings,
	}, nil
}

// One streaming completion; onToken fires per delta as it arrives.
func Stream(ctx context.Context, url, key, model string, messages []Message, maxTokens int, onToken func(string)) (*Reply, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	})
	resp, err := Post(ctx, url+"/chat/completions", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Error(resp)
	}
	var text strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64*MaxBody))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxBody)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("the model server failed: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				onToken(choice.Delta.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("the reply stream broke: %w", err)
	}
	return &Reply{Text: text.String()}, nil
}

func Post(ctx context.Context, url, key string, body []byte) (*http.Response, error) {
	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{Proxy: nil},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the model server: %w", err)
	}
	return resp, nil
}

func Error(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, parsed.Error.Message)
	}
	if len(body) > 0 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// An image the model can look at: data URL part for chat completions.
func ImagePart(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	mime := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
	}[strings.ToLower(filepath.Ext(path))]
	if mime == "" {
		return nil, fmt.Errorf("%s is not a supported image (png, jpg, webp, gif)", path)
	}
	return map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url": fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data)),
		},
	}, nil
}

// An audio take the model can hear: input_audio part for chat completions.
func AudioPart(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if format == "" {
		return nil, fmt.Errorf("%s has no audio extension (wav, mp3, flac, m4a, ogg)", path)
	}
	return map[string]any{
		"type": "input_audio",
		"input_audio": map[string]any{
			"data":   base64.StdEncoding.EncodeToString(data),
			"format": format,
		},
	}, nil
}

// Embed makes one /v1/embeddings call and returns the pooled vector.
func Embed(ctx context.Context, url, key, model, text string) ([]float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"input": text,
	})
	resp, err := Post(ctx, url+"/embeddings", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Error(resp)
	}
	var parsed struct {
		Data []struct {
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("the server returned no embedding")
	}
	var vec []float64
	if err := json.Unmarshal(parsed.Data[0].Embedding, &vec); err == nil {
		return vec, nil
	}
	// Some builds wrap the pooled vector in an extra array.
	var rows [][]float64
	if err := json.Unmarshal(parsed.Data[0].Embedding, &rows); err == nil && len(rows) == 1 {
		return rows[0], nil
	}
	return nil, fmt.Errorf("the server returned an embedding shape fornax cannot read")
}

// GetJSON is a keyed GET that decodes a JSON object; nil means the server
// is down, refused the key, or answered something else.
func GetJSON(client *http.Client, url, key string) map[string]any {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	return value
}

// A typed question for a decision model's /v1/systemone endpoint (TypeSafe's
// System One API): a yes/no (noul), one of a set (choice), or a level on an
// ordered scale (score).
type TypedQuestion struct {
	Kind         string
	Instructions string
	Options      []string
}

func (q *TypedQuestion) wire() map[string]any {
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

// SystemOne asks a decision model typed questions about a piece of state and
// returns the raw reply: answers, probabilities, legend and latency_ms.
func SystemOne(ctx context.Context, url, key, model, state string, questions map[string]*TypedQuestion) (map[string]any, error) {
	qs := map[string]any{}
	for id, q := range questions {
		qs[id] = q.wire()
	}
	body, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": qs})
	resp, err := Post(ctx, url+"/systemone", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Error(resp)
	}
	var parsed map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	return parsed, nil
}

// One scored document, in the server's input indexing.
type RerankHit struct {
	Index int
	Score float64
	Text  string
}

// Rerank scores docs against a query over /v1/rerank, best first. The server
// reports only an index and score unless return_text is set, so the row text
// falls back to the input doc by index.
func Rerank(ctx context.Context, url, key, model, query string, docs []string, topN int) ([]RerankHit, error) {
	req := map[string]any{"model": model, "query": query, "documents": docs}
	if topN > 0 {
		req["top_n"] = topN
	}
	body, _ := json.Marshal(req)
	resp, err := Post(ctx, url+"/rerank", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Error(resp)
	}
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Document       *struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("the server returned no rankings")
	}
	hits := make([]RerankHit, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		hit := RerankHit{Index: r.Index, Score: r.RelevanceScore}
		if r.Document != nil && r.Document.Text != "" {
			hit.Text = r.Document.Text
		} else {
			if r.Index < 0 || r.Index >= len(docs) {
				return nil, fmt.Errorf("the server returned a result index out of range")
			}
			hit.Text = docs[r.Index]
		}
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, nil
}
