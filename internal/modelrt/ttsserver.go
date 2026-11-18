package modelrt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/openai"
	"github.com/earshot-run/fornax/internal/paths"
)

var speechClient = &http.Client{
	Timeout:   10 * time.Minute,
	Transport: &http.Transport{},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func speechServed(spec *catalog.Spec, port int, key string) bool {
	probe := *speechClient
	probe.Timeout = 3 * time.Second
	models := openai.GetJSON(&probe, paths.EndpointURL(port)+"/models", key)
	rows, _ := models["data"].([]any)
	for _, row := range rows {
		model, _ := row.(map[string]any)
		if model["id"] != spec.ID {
			continue
		}
		arch, _ := model["architecture"].(map[string]any)
		outputs, _ := arch["output_modalities"].([]any)
		return slices.Contains(outputs, any("audio"))
	}
	return false
}

func speechURL(port int) string {
	return strings.TrimSuffix(paths.EndpointURL(port), "/v1") + "/tts"
}

func requestSpeech(ctx context.Context, port int, key, prompt, out, voice, lang string, frames int) error {
	var ref []byte
	if voice != "" {
		var err error
		ref, err = os.ReadFile(voice)
		if err != nil {
			return fmt.Errorf("could not read the reference voice: %w", err)
		}
		if len(ref) == 0 {
			return fmt.Errorf("the reference voice %s is empty", voice)
		}
	}
	body, err := json.Marshal(struct {
		Input      string `json:"input"`
		Language   string `json:"lang"`
		SpeakerRef []byte `json:"speaker_ref_b64,omitempty"`
		Frames     int    `json:"n_predict,omitempty"`
	}{prompt, lang, ref, max(frames, 0)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, speechURL(port), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := speechClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the speech server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return openai.Error(resp)
	}
	return saveSpeechWAV(out, resp.Body)
}

func saveSpeechWAV(out string, audio io.Reader) error {
	var header [12]byte
	if _, err := io.ReadFull(audio, header[:]); err != nil {
		return fmt.Errorf("could not read the speech WAV header: %w", err)
	}
	size := binary.LittleEndian.Uint32(header[4:8])
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" || size <= 36 {
		return fmt.Errorf("the speech server did not return a WAV with audio")
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".fornax-speech-*.wav")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(header[:]); err != nil {
		return err
	}
	n, err := io.Copy(tmp, audio)
	if err != nil {
		return fmt.Errorf("could not read the speech WAV: %w", err)
	}
	if n+4 != int64(size) {
		return fmt.Errorf("the speech server returned an incomplete WAV")
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}
