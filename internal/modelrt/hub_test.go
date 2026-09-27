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
		if r.URL.Query().Get("search") != "qwen" || r.URL.Query().Get("full") != "false" {
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
