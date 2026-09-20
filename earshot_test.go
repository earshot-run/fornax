package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The daemon's contract: Bearer control token, POST /v1/local-models/manage,
// camelCase {action:"connect", url, apiKey}; 200 means registered.
func TestConnectPostsTheDaemonContract(t *testing.T) {
	var seen map[string]string
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/local-models/manage" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		token = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&seen)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"connections": []any{}})
	}))
	defer server.Close()

	home := t.TempDir()
	connection := fmt.Sprintf(
		`{"version":2,"surface":"control","url":%q,"token":"escc_test"}`, server.URL)
	os.WriteFile(filepath.Join(home, "control.json"), []byte(connection), 0o600)
	t.Setenv("EARSHOT_HOME", home)

	result, detail := connectEarshot("http://127.0.0.1:7331/v1", "esk_local_key")
	if result != connectRegistered {
		t.Fatalf("connect = %v (%s), want registered", result, detail)
	}
	if token != "Bearer escc_test" {
		t.Errorf("authorization = %q", token)
	}
	if seen["action"] != "connect" || seen["url"] != "http://127.0.0.1:7331/v1" || seen["apiKey"] != "esk_local_key" {
		t.Errorf("body = %v", seen)
	}
}

func TestConnectReportsADaemonRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown variant `connect`"})
	}))
	defer server.Close()

	home := t.TempDir()
	connection := fmt.Sprintf(
		`{"version":2,"surface":"control","url":%q,"token":"escc_test"}`, server.URL)
	os.WriteFile(filepath.Join(home, "control.json"), []byte(connection), 0o600)
	t.Setenv("EARSHOT_HOME", home)

	result, detail := connectEarshot("http://127.0.0.1:7331/v1", "esk_local_key")
	if result != connectUnavailable {
		t.Fatalf("connect = %v, want unavailable", result)
	}
	if detail == "" {
		t.Fatal("a refused connect reported no detail")
	}
}

func TestConnectWithoutADaemonSaysSo(t *testing.T) {
	t.Setenv("EARSHOT_HOME", t.TempDir())
	result, _ := connectEarshot("http://127.0.0.1:7331/v1", "esk_local_key")
	if result != connectNoDaemon {
		t.Fatalf("connect = %v, want no daemon", result)
	}
}
