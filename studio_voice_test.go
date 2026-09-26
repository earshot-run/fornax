package main

import (
	"strings"
	"testing"
)

func TestStudioSpeechValidates(t *testing.T) {
	s := testStudio(t)
	validate := s.kinds["speech"].validate
	cases := map[string]studioRequest{
		"not one of":  {Prompt: "hi", Lang: "tlh"},
		"characters":  {Prompt: strings.Repeat("a", studioSpeechMaxChars+1)},
		"WAV or MP3":  {Prompt: "hi", Voice: "0123456789abcdef.ogg"},
		"WAV or MP3 ": {Prompt: "hi", Voice: "0123456789abcdef.png"},
	}
	for want, req := range cases {
		if err := validate(&req); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%+v: err = %v, want it to mention %q", req, err, want)
		}
	}
	req := studioRequest{Prompt: "hi", Voice: "0123456789abcdef.wav", Width: 512, Steps: 20, Refs: []string{"x.png"}, Negative: "n"}
	if err := validate(&req); err != nil {
		t.Fatal(err)
	}
	if req.Lang != "en" || req.Width != 0 || req.Steps != 0 || req.Refs != nil || req.Negative != "" {
		t.Errorf("speech kept image fields or no default language: %+v", req)
	}
}

func TestStudioSpeechProgressFollowsTTSOutput(t *testing.T) {
	s := testStudio(t)
	job := &studioJob{studioItem: studioItem{ID: "0123456789abcdef", Kind: "speech", Ext: "wav"}, State: jobRunning, Phase: "loading"}
	s.speechProgress(job, "0.01.401.327 I cmn          init: llama threadpool init, n_threads = 8")
	if job.Phase != "loading" {
		t.Errorf("phase after load line = %q", job.Phase)
	}
	s.speechProgress(job, "0.02.274.592 W init: embeddings required but some input tokens were not marked as outputs -> overriding")
	if job.Phase != "speaking" {
		t.Errorf("phase once decoding starts = %q, want speaking", job.Phase)
	}
	s.speechProgress(job, "0.04.486.773 I generated 39 frames, 149804 bytes of WAV audio (24000 Hz)")
	if job.Phase != "saving" {
		t.Errorf("phase after the last frame = %q, want saving", job.Phase)
	}
}

func TestStudioVideoTakesOneStartImage(t *testing.T) {
	s := testStudio(t)
	validate := s.kinds["video"].validate
	base := studioRequest{Width: 480, Height: 272, Steps: 20, Frames: 33}
	two := base
	two.Refs = []string{"0123456789abcdef.png", "fedcba9876543210.png"}
	if err := validate(&two); err == nil || !strings.Contains(err.Error(), "one image") {
		t.Errorf("two start images: err = %v", err)
	}
	audio := base
	audio.Refs = []string{"0123456789abcdef.wav"}
	if err := validate(&audio); err == nil || !strings.Contains(err.Error(), "PNG") {
		t.Errorf("audio as a start image: err = %v", err)
	}
	one := base
	one.Refs = []string{"0123456789abcdef.jpg"}
	if err := validate(&one); err != nil {
		t.Errorf("one start image: %v", err)
	}
}

func TestStudioVideoPreviewIsAnimatedWebP(t *testing.T) {
	s := testStudio(t)
	video := &studioItem{ID: "0123456789abcdef", Kind: "video"}
	image := &studioItem{ID: "0123456789abcdef", Kind: "image"}
	if !strings.HasSuffix(s.previewPath(video), ".webp") || !strings.HasSuffix(s.previewPath(image), ".png") {
		t.Errorf("preview paths: video %s, image %s", s.previewPath(video), s.previewPath(image))
	}
}
