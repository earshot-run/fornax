package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseFlexibleTakesPositionalsBeforeOrAfterFlags(t *testing.T) {
	for _, args := range [][]string{
		{"one", "two", "-n", "3"},
		{"-n", "3", "one", "two"},
		{"one", "-n", "3", "two"},
	} {
		set := flag.NewFlagSet("t", flag.ContinueOnError)
		n := set.Int("n", 0, "")
		got := parseFlexible(set, args, 2)
		if strings.Join(got, " ") != "one two" || *n != 3 {
			t.Errorf("args %v: got %v with -n %d", args, got, *n)
		}
	}
	set := flag.NewFlagSet("t", flag.ContinueOnError)
	events := set.Bool("events", false, "")
	got := parseFlexible(set, []string{"a", "b", "--events"}, -1)
	if strings.Join(got, " ") != "a b" || !*events {
		t.Errorf("an unlimited lead takes every id: got %v with --events %v", got, *events)
	}
}

// The usage line documents the reference first, so running it that way has
// to work. A vision pull against a repo with no projector fails clean.
func TestPullHFAcceptsFlagsAfterTheReference(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			w.Write([]byte(`{"siblings":[{"rfilename":"File.gguf"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	old := hfHost
	hfHost = srv.URL
	defer func() { hfHost = old }()
	t.Setenv("FORNAX_HOME", t.TempDir())
	for _, args := range [][]string{
		{"hf:Org/Repo/File.gguf", "--kind", "vision"},
		{"--kind", "vision", "hf:Org/Repo/File.gguf"},
	} {
		err := cmdPullHF(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "--mmproj") {
			t.Errorf("args %v: want the missing-projector error, got %v", args, err)
		}
	}
}
