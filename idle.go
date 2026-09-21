package main

// `run -idle 20m` stops a served model that has done no work for that long.
// llama-server exposes monotonic counters on `/metrics`, so a request that
// started and finished between two polls still counts as activity.

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const idlePoll = 15 * time.Second

var idleCounters = []string{
	"llamacpp:prompt_tokens_total",
	"llamacpp:tokens_predicted_total",
	"llamacpp:n_decode_total",
}

func metricsFingerprint(client *http.Client, port int, key string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	if err != nil {
		return "", false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	return fingerprint(bufio.NewScanner(resp.Body)), true
}

func fingerprint(lines *bufio.Scanner) string {
	var kept []string
	for lines.Scan() {
		line := lines.Text()
		for _, counter := range idleCounters {
			if strings.HasPrefix(line, counter+" ") {
				kept = append(kept, line)
			}
		}
		if strings.HasPrefix(line, "llamacpp:requests_processing ") && !strings.HasSuffix(line, " 0") {
			// A request in flight is activity even before it produces a token.
			kept = append(kept, line+time.Now().String())
		}
	}
	return strings.Join(kept, "\n")
}

// Fires once `sample` has returned the same fingerprint for `limit`. A
// failed sample counts as activity: never stop a server we cannot read.
func idleAfter(limit time.Duration, poll time.Duration, sample func() (string, bool), done <-chan struct{}) <-chan struct{} {
	fired := make(chan struct{})
	if limit <= 0 {
		return fired
	}
	go func() {
		last, _ := sample()
		since := time.Now()
		tick := time.NewTicker(poll)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			current, ok := sample()
			if !ok || current != last {
				last, since = current, time.Now()
				continue
			}
			if time.Since(since) >= limit {
				close(fired)
				return
			}
		}
	}()
	return fired
}
