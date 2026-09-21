package main

import (
	"bufio"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleFiresOnlyAfterTheCountersStopMoving(t *testing.T) {
	var work atomic.Int64
	done := make(chan struct{})
	defer close(done)
	sample := func() (string, bool) { return string(rune('a' + work.Load())), true }
	fired := idleAfter(60*time.Millisecond, 10*time.Millisecond, sample, done)
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		work.Add(1)
		select {
		case <-fired:
			t.Fatal("stopped a model that was still answering")
		default:
		}
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("never stopped an idle model")
	}
}

func TestIdleNeverStopsAServerItCannotRead(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	fired := idleAfter(30*time.Millisecond, 5*time.Millisecond, func() (string, bool) { return "", false }, done)
	select {
	case <-fired:
		t.Fatal("an unreadable server was treated as idle")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestFingerprintMovesWithTokensAndInFlightRequests(t *testing.T) {
	read := func(body string) string { return fingerprint(bufio.NewScanner(strings.NewReader(body))) }
	quiet := "llamacpp:prompt_tokens_total 10\nllamacpp:requests_processing 0\nother 5\n"
	if read(quiet) != read(quiet) {
		t.Fatal("an idle server must fingerprint the same twice")
	}
	if read(quiet) == read("llamacpp:prompt_tokens_total 11\nllamacpp:requests_processing 0\n") {
		t.Fatal("new tokens must change the fingerprint")
	}
	busy := "llamacpp:prompt_tokens_total 10\nllamacpp:requests_processing 1\n"
	first := read(busy)
	time.Sleep(time.Millisecond)
	if first == read(busy) {
		t.Fatal("a request in flight must count as activity")
	}
}
