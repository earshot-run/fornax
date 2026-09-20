package main

// Talking to the running llama-server: one chat-completions client that both
// the one-shot commands and the interactive REPL share. Content is text by
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
	"strings"
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func textMessage(role, text string) message {
	return message{Role: role, Content: text}
}

type chatUsage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	PredictedPerSec  float64 `json:"predicted_per_second"`
}

type chatReply struct {
	Text    string
	Usage   *chatUsage
	Timings map[string]any
}

// One non-streaming completion. `content` is a plain string or a parts array.
func chatOnce(ctx context.Context, url, key, model string, messages []message, maxTokens int) (*chatReply, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     false,
	})
	resp, err := post(ctx, url+"/chat/completions", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage   *chatUsage     `json:"usage"`
		Timings map[string]any `json:"timings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPBody)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("the reply was not readable: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("the server returned no reply")
	}
	return &chatReply{
		Text:    parsed.Choices[0].Message.Content,
		Usage:   parsed.Usage,
		Timings: parsed.Timings,
	}, nil
}

// One streaming completion; onToken fires per delta as it arrives.
func chatStream(ctx context.Context, url, key, model string, messages []message, maxTokens int, onToken func(string)) (*chatReply, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	})
	resp, err := post(ctx, url+"/chat/completions", key, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var text strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64*maxHTTPBody))
	scanner.Buffer(make([]byte, 0, 64*1024), maxHTTPBody)
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
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
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
	return &chatReply{Text: text.String()}, nil
}

func post(ctx context.Context, url, key string, body []byte) (*http.Response, error) {
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

func apiError(resp *http.Response) error {
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
func imagePart(path string) (map[string]any, error) {
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
func audioPart(path string) (map[string]any, error) {
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
