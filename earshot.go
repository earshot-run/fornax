package main

// Handing a running server to Earshot. The daemon keeps a private control
// capability in `~/.earshot/control.json`; one POST to
// `/v1/local-models/manage` makes it probe `/v1/models` and offer the model
// in Settings ▸ Local models. Anything short of that falls back to the same
// values as paste-in-Earshot instructions — the operator path needs no token.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/ui"
)

type localConnection struct {
	Version int    `json:"version"`
	Surface string `json:"surface"`
	URL     string `json:"url"`
	Token   string `json:"token"`
}

func earshotHome() string {
	if dir := os.Getenv("EARSHOT_HOME"); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".earshot")
	}
	return ".earshot"
}

func controlConnection() *localConnection {
	data, err := os.ReadFile(filepath.Join(earshotHome(), "control.json"))
	if err != nil {
		return nil
	}
	var connection localConnection
	if json.Unmarshal(data, &connection) != nil {
		return nil
	}
	if connection.Version != 2 || connection.Surface != "control" || connection.Token == "" {
		return nil
	}
	return &connection
}

type connectResult int

const (
	// The daemon accepted the URL; the model is in the Earshot picker.
	connectRegistered connectResult = iota
	// Earshot answered but would not connect (older build, refused URL).
	connectUnavailable
	// No daemon to talk to.
	connectNoDaemon
)

// Register `url` (already serving, key enforced) with a live Earshot daemon.
// The detail explains anything that is not Registered.
func connectEarshot(url, apiKey string) (connectResult, string) {
	connection := controlConnection()
	if connection == nil {
		return connectNoDaemon, ""
	}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	body, _ := json.Marshal(map[string]string{
		"action": "connect",
		"url":    url,
		"apiKey": apiKey,
	})
	req, err := http.NewRequest(http.MethodPost, connection.URL+"/v1/local-models/manage", bytes.NewReader(body))
	if err != nil {
		return connectNoDaemon, ""
	}
	req.Header.Set("Authorization", "Bearer "+connection.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return connectNoDaemon, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return connectRegistered, ""
	}
	var parsed struct {
		Error string `json:"error"`
	}
	var detail string
	var raw [8192]byte
	n, _ := resp.Body.Read(raw[:])
	if json.Unmarshal(raw[:n], &parsed) == nil && parsed.Error != "" {
		detail = parsed.Error
	} else if n > 0 {
		detail = string(raw[:n])
	}
	return connectUnavailable, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, detail)
}

// The values an operator pastes into Settings ▸ Local models, or feeds any
// other OpenAI-compatible client.
func pasteBlock(url, apiKey, model string) string {
	return fmt.Sprintf("    %s\n      %s %s\n      %s %s\n      %s %s",
		ui.Dim("earshot → Settings → Local models → connect a server"),
		ui.Dim("url:"), url,
		ui.Dim("key:"), apiKey,
		ui.Dim("model:"), model)
}

// llama-server also answers Anthropic's /v1/messages — the same key goes in
// x-api-key. The env-var pair is what Claude-flavored clients read.
func anthropicLine(url, apiKey string) string {
	return fmt.Sprintf("    %s %s %s",
		ui.Dim("anthropic →"),
		ui.Dim("ANTHROPIC_BASE_URL="+strings.TrimSuffix(url, "/v1")),
		ui.Dim("ANTHROPIC_API_KEY="+apiKey))
}

func earshotPresent() bool {
	for _, name := range []string{"daemon.owner", "control.json"} {
		if _, err := os.Stat(filepath.Join(earshotHome(), name)); err == nil {
			return true
		}
	}
	return false
}

func reportConnect(url, apiKey, model string) {
	result, detail := connectEarshot(url, apiKey)
	switch result {
	case connectRegistered:
		fmt.Printf("    %s connected — pick %s in the agent's model list\n", ui.Green("earshot:"), ui.Bold("'"+model+"'"))
	case connectUnavailable:
		fmt.Printf("    %s daemon would not connect (%s)\n", ui.Yellow("earshot:"), ui.Dim(detail))
		fmt.Println(pasteBlock(url, apiKey, model))
	case connectNoDaemon:
		fmt.Printf("    %s no daemon found on this computer\n", ui.Dim("earshot:"))
		fmt.Println(pasteBlock(url, apiKey, model))
	}
}
