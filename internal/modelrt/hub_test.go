package modelrt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPopularHFListsGGUFsByDownloads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("sort") != "downloads" || q.Get("direction") != "-1" || q.Get("filter") != "gguf" || q.Get("full") != "true" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[
			{"id":"org/Smaller","downloads":2,"likes":1,"siblings":[{"rfilename":"a.gguf"},{"rfilename":"README.md"}]},
			{"id":"org/Bigger","downloads":9,"likes":3,"siblings":[{"rfilename":"b.gguf"},{"rfilename":"c.gguf"}]}
		]`))
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	defer func() { HFHost = old }()

	hits, err := PopularHF(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Repo != "org/Bigger" || len(hits[0].GGUFs) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestPopularHFFollowsTheKindAndSkipsBundles(t *testing.T) {
	var pipes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pipes = append(pipes, r.URL.Query().Get("pipeline_tag"))
		w.Write([]byte(`[
			{"id":"org/Bundle","downloads":99,"siblings":[
				{"rfilename":"a/f.gguf"},{"rfilename":"b/f.gguf"},{"rfilename":"c/f.gguf"},
				{"rfilename":"d/f.gguf"},{"rfilename":"e/f.gguf"},{"rfilename":"f/f.gguf"},
				{"rfilename":"g/f.gguf"},{"rfilename":"h/f.gguf"},{"rfilename":"i/f.gguf"}
			]},
			{"id":"org/Picture","downloads":5,"siblings":[{"rfilename":"pic.gguf"}]}
		]`))
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	defer func() { HFHost = old }()

	hits, err := PopularHF(context.Background(), "image", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pipes, ",") != "text-to-image,image-to-image" {
		t.Fatalf("pipelines = %v", pipes)
	}
	if len(hits) != 1 || hits[0].Repo != "org/Picture" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchHFSendsTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("search") != "qwen" || r.URL.Query().Get("full") != "false" || r.URL.Query().Get("sort") != "downloads" || r.URL.Query().Get("direction") != "-1" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"id":"org/Qwen","downloads":1,"siblings":[{"rfilename":"q.gguf"}]}]`))
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	defer func() { HFHost = old }()

	hits, err := SearchHF(context.Background(), "qwen", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.HasSuffix(hits[0].GGUFs[0], ".gguf") {
		t.Fatalf("hits = %+v", hits)
	}
}

// New discovery must preserve upstream recency order even when an older model
// has more downloads; popularity remains the CLI's default.
func TestHFDiscoverySort(t *testing.T) {
	t.Setenv("FORNAX_HOME", t.TempDir())
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_ENDPOINT", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") != "lastModified" || r.URL.Query().Get("direction") != "-1" {
			t.Errorf("sort query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[
			{"id":"org/Old","downloads":900,"lastModified":"2026-01-01T00:00:00Z","siblings":[{"rfilename":"old.gguf"}]},
			{"id":"org/New","downloads":2,"lastModified":"2026-10-01T00:00:00Z","siblings":[{"rfilename":"new.gguf"}]}
		]`))
	}))
	defer srv.Close()
	old := HFHost
	HFHost = srv.URL
	t.Cleanup(func() { HFHost = old })
	for _, search := range []bool{false, true} {
		var hits []SearchHit
		var err error
		if search {
			hits, err = SearchHFWithSort(context.Background(), "new", 1, "lastModified")
		} else {
			hits, err = PopularHFWithSort(context.Background(), "image", 1, "lastModified")
		}
		if err != nil || len(hits) != 1 || hits[0].Repo != "org/New" || hits[0].Updated.IsZero() {
			t.Fatalf("search=%v: hits=%+v, err=%v", search, hits, err)
		}
	}
	if _, err := SearchHFWithSort(context.Background(), "new", 1, "invalid"); err == nil {
		t.Fatal("invalid search sort accepted")
	}
	if _, err := PopularHFWithSort(context.Background(), "", 1, "invalid"); err == nil {
		t.Fatal("invalid discovery sort accepted")
	}
}
