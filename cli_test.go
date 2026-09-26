package main

import (
	"strings"
	"testing"
)

// The usage text is hand-written; the dispatch table is the truth. A command
// that never reaches `fornax -h` is one nobody finds.
func TestUsageAndDispatchTableAgree(t *testing.T) {
	listed := map[string]bool{}
	for _, line := range strings.Split(usage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		if name, _, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			listed[name] = true
		}
	}
	known := map[string]bool{}
	for _, command := range commands() {
		known[command.name] = true
		if strings.HasPrefix(command.name, "__") {
			continue
		}
		if !listed[command.name] {
			t.Errorf("`fornax -h` never mentions %s", command.name)
		}
	}
	for name := range listed {
		if !known[name] {
			t.Errorf("`fornax -h` lists %s, which is not a command", name)
		}
	}
	for _, name := range strings.Fields(completionCommands()) {
		if !known[name] {
			t.Errorf("completion offers %s, which is not a command", name)
		}
	}
}

func TestDidYouMeanOnlyGuessesWhenItIsClose(t *testing.T) {
	for typo, want := range map[string]string{"lst": "list", "puul": "pull", "conect": "connect", "seach": "search"} {
		if got := didYouMean(typo); !strings.Contains(got, want) {
			t.Errorf("didYouMean(%q) = %q, want a nudge toward %s", typo, got, want)
		}
	}
	for _, nonsense := range []string{"xyzzy", "instal", "x", "somethingelse"} {
		if got := didYouMean(nonsense); got != "" {
			t.Errorf("didYouMean(%q) = %q, want silence", nonsense, got)
		}
	}
}
