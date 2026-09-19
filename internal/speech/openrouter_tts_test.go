package speech

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

func TestOpenRouterSynthesizerRequestsStreamingPCMAndFramesWAV(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		var request struct {
			Model      string   `json:"model"`
			Modalities []string `json:"modalities"`
			Audio      struct {
				Voice  string `json:"voice"`
				Format string `json:"format"`
			} `json:"audio"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "openai/gpt-audio-mini" || request.Audio.Voice != "marin" || request.Audio.Format != "pcm16" || !request.Stream {
			t.Errorf("request = %+v", request)
		}
		if len(request.Modalities) != 2 || request.Modalities[1] != "audio" {
			t.Errorf("modalities = %v", request.Modalities)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":%q,\"transcript\":\"Hello \"}}}]}\n\n", base64.StdEncoding.EncodeToString(pcm[:4]))
		writeSSE(t, w, "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":%q,\"transcript\":\"world.\"}}}]}\n\n", base64.StdEncoding.EncodeToString(pcm[4:]))
		writeSSE(t, w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	synth := NewOpenRouterSynthesizer(server.URL, "openai/gpt-audio-mini", "marin", "secret", time.Second, nil)
	synth.Client = server.Client()
	wav, err := synth.Synthesize(context.Background(), "Hello world.")
	if err != nil {
		t.Fatal(err)
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("not a WAV: %q", wav[:12])
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 24000 {
		t.Fatalf("sample rate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Fatalf("channels = %d", got)
	}
	if got := wav[44:]; string(got) != string(pcm) {
		t.Fatalf("PCM = %v, want %v", got, pcm)
	}
}

func TestChangedOpenRouterSpeechFallsBackInsteadOfSpeakingIt(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded := base64.StdEncoding.EncodeToString([]byte{1, 0})
		writeSSE(t, w, "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":%q,\"transcript\":\"Sure, starting music.\"}}}]}\n\n", encoded)
		writeSSE(t, w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	remote := NewOpenRouterSynthesizer(server.URL, "openai/gpt-audio-mini", "marin", "secret", time.Second, nil)
	remote.Client = server.Client()
	local := &stubSynthesizer{name: "piper", wav: audio.EncodeWAV([]int16{9}, audio.Default())}

	cascade := NewSynthesizerCascade(nil, remote, local)
	wav, err := cascade.Synthesize(context.Background(), "Starting music.")
	if err != nil {
		t.Fatal(err)
	}
	if local.calls != 1 || string(wav) != string(local.wav) {
		t.Fatalf("local calls=%d; changed remote speech was not replaced", local.calls)
	}
}

func TestSynthesizerCascadeFallsBackWhenRemoteIsUnavailable(t *testing.T) {
	remote := &stubSynthesizer{name: "remote", err: errors.New("network down")}
	local := &stubSynthesizer{name: "piper", wav: audio.EncodeWAV([]int16{7}, audio.Default())}
	wav, err := NewSynthesizerCascade(nil, remote, local).Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if remote.calls != 1 || local.calls != 1 || string(wav) != string(local.wav) {
		t.Fatalf("remote=%d local=%d wav_match=%v", remote.calls, local.calls, string(wav) == string(local.wav))
	}
}

func TestOpenRouterSynthesizerRejectsMissingKeyBeforeNetwork(t *testing.T) {
	synth := NewOpenRouterSynthesizer("https://openrouter.ai/api/v1/chat/completions", "model", "marin", "", time.Second, nil)
	_, err := synth.Synthesize(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("error = %v", err)
	}
}

func writeSSE(t *testing.T, w http.ResponseWriter, format string, args ...any) {
	t.Helper()
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		t.Error(err)
	}
}

type stubSynthesizer struct {
	name  string
	wav   []byte
	err   error
	calls int
}

func (s *stubSynthesizer) Synthesize(context.Context, string) ([]byte, error) {
	s.calls++
	return s.wav, s.err
}
func (s *stubSynthesizer) Name() string              { return s.name }
func (s *stubSynthesizer) Available() (bool, string) { return s.err == nil, s.name }
