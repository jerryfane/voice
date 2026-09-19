package speech

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseAudio builds a response in the shape OpenRouter actually streams:
// base64 pcm16 in delta.audio.data, spread over several events.
func sseAudio(t *testing.T, samples []int16, transcript string, chunks int) string {
	t.Helper()
	raw := make([]byte, 0, len(samples)*2)
	for _, s := range samples {
		raw = append(raw, byte(uint16(s)), byte(uint16(s)>>8))
	}
	var b strings.Builder
	per := len(raw) / chunks
	for i := 0; i < chunks; i++ {
		start := i * per
		end := start + per
		if i == chunks-1 {
			end = len(raw)
		}
		ev := map[string]any{"choices": []map[string]any{{"delta": map[string]any{
			"audio": map[string]string{"data": base64.StdEncoding.EncodeToString(raw[start:end])},
		}}}}
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "data: %s\n\n", line)
	}
	ev := map[string]any{"choices": []map[string]any{{"delta": map[string]any{
		"audio": map[string]string{"transcript": transcript},
	}}}}
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "data: %s\n\ndata: [DONE]\n\n", line)
	return b.String()
}

func newTestSynth(t *testing.T, handler http.HandlerFunc) (*OpenRouterSynthesizer, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := NewOpenRouterSynthesizer(srv.URL, "openai/gpt-audio-mini", "marin", "test-key", 10*time.Second, nil)
	return s, srv
}

// The audio arrives as headerless pcm16 spread over many SSE events, because
// streaming is the only mode that returns audio at all and wav is refused.
// Reassembling it wrongly - dropping events, or mis-framing the WAV - is the
// difference between speech and noise.
func TestSynthesizeReassemblesStreamedAudioIntoPlayableWAV(t *testing.T) {
	want := []int16{0, 1000, -1000, 32767, -32768, 12345}
	var gotBody map[string]any
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseAudio(t, want, "hello there", 3))
	})

	wav, err := s.Synthesize(context.Background(), "hello there")
	if err != nil {
		t.Fatal(err)
	}

	// A WAV the player can actually open: RIFF header, and every sample.
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("output is not a WAV: first bytes %q", wav[:min(16, len(wav))])
	}
	if got, wantBytes := len(wav)-44, len(want)*2; got != wantBytes {
		t.Errorf("payload = %d bytes, want %d: audio was lost or duplicated across chunks", got, wantBytes)
	}
	for i, s16 := range want {
		lo, hi := wav[44+2*i], wav[44+2*i+1]
		if got := int16(uint16(lo) | uint16(hi)<<8); got != s16 {
			t.Errorf("sample %d = %d, want %d", i, got, s16)
		}
	}

	// The request must ask for the streamed pcm16 form, because the plain
	// one is refused outright with "Audio output requires stream: true" and
	// wav is refused when streaming.
	if gotBody["stream"] != true {
		t.Error("request did not set stream:true; OpenRouter refuses audio without it")
	}
	audioReq, _ := gotBody["audio"].(map[string]any)
	if audioReq["format"] != "pcm16" {
		t.Errorf("audio.format = %v, want pcm16 (wav is rejected while streaming)", audioReq["format"])
	}
	if audioReq["voice"] != "marin" {
		t.Errorf("audio.voice = %v, want the configured voice", audioReq["voice"])
	}
}

// An HTTP error must carry the provider's explanation. "400" alone sent me
// round the houses twice today; the body said exactly what was wrong.
func TestSynthesizeReportsWhyTheProviderRefused(t *testing.T) {
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Unsupported value: 'audio.format' does not support 'wav' when stream=true"}}`)
	})

	_, err := s.Synthesize(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not support 'wav'") {
		t.Errorf("error %q does not carry the provider's explanation", err)
	}
}

// An error delivered INSIDE the stream, after a 200, must not be mistaken for
// a short but successful reply.
func TestSynthesizeFailsOnAnErrorCarriedInsideTheStream(t *testing.T) {
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"rate limited\"}}\n\ndata: [DONE]\n\n")
	})

	_, err := s.Synthesize(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v, want the in-stream failure surfaced", err)
	}
}

// A 200 with no audio is a failure, not silence. Returning an empty WAV would
// make the assistant mute while reporting success.
func TestSynthesizeFailsWhenNoAudioArrives(t *testing.T) {
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"sure thing\"}}]}\n\ndata: [DONE]\n\n")
	})

	_, err := s.Synthesize(context.Background(), "hello")
	if err == nil {
		t.Fatal("a response carrying no audio must be an error, or the assistant goes silently mute")
	}
}

// These models are conversational: asked to say a line, they sometimes say
// something else. The audio is still played - silence would be worse - but
// the drift has to be reported, or an assistant improvising words nobody gave
// it goes unnoticed.
func TestSynthesizeReportsWhenTheModelSaysSomethingElse(t *testing.T) {
	var logged []string
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sseAudio(t, []int16{1, 2, 3, 4}, "Sure! The shop closes at six.", 1))
	})
	s.Logger = loggerFunc(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	wav, err := s.Synthesize(context.Background(), "The shop closes at six")
	if err != nil {
		t.Fatal(err)
	}
	if len(wav) <= 44 {
		t.Error("drift must not suppress the audio; a spoken answer beats silence")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "drift") {
		t.Errorf("logged %v, want one drift report", logged)
	}
}

// ...and must NOT cry wolf on differences no listener can hear, or the report
// becomes noise and gets ignored.
func TestSynthesizeIgnoresPunctuationAndCaseDifferences(t *testing.T) {
	var logged []string
	s, _ := newTestSynth(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sseAudio(t, []int16{1, 2, 3, 4}, "  the shop closes at SIX!  ", 1))
	})
	s.Logger = loggerFunc(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})

	if _, err := s.Synthesize(context.Background(), "The shop closes at six."); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 0 {
		t.Errorf("reported drift for a cosmetic difference: %v", logged)
	}
}

type loggerFunc func(string, ...any)

func (f loggerFunc) Printf(format string, args ...any) { f(format, args...) }

type stubSynth struct {
	name string
	wav  []byte
	err  error
}

func (s stubSynth) Name() string              { return s.name }
func (s stubSynth) Available() (bool, string) { return s.err == nil, s.name }
func (s stubSynth) Synthesize(context.Context, string) ([]byte, error) {
	return s.wav, s.err
}

// A network blip must not leave the assistant mute: the local engine is there
// precisely so a failed cloud call still produces speech.
func TestCascadeFallsBackToTheLocalVoice(t *testing.T) {
	local := stubSynth{name: "piper", wav: []byte("local-audio")}
	remote := stubSynth{name: "openrouter", err: errors.New("dial tcp: network is unreachable")}

	var logged []string
	c := NewSynthesizerCascade(loggerFunc(func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	}), remote, local)

	got, err := c.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("cascade failed while a working engine remained: %v", err)
	}
	if string(got) != "local-audio" {
		t.Errorf("got %q, want the local engine's audio", got)
	}
	if len(logged) == 0 {
		t.Error("a silent fallback hides that the chosen voice is not being used")
	}
}

// An engine that returns success and no audio is broken, and must not end the
// cascade - that would be mute-but-happy, the exact failure the fallback is
// there to prevent.
func TestCascadeTreatsEmptyAudioAsFailure(t *testing.T) {
	empty := stubSynth{name: "openrouter", wav: nil}
	local := stubSynth{name: "piper", wav: []byte("local-audio")}
	c := NewSynthesizerCascade(nil, empty, local)

	got, err := c.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local-audio" {
		t.Errorf("got %q, want the fallback to run after empty audio", got)
	}
}

// With every engine down the caller must hear WHY, naming each attempt, not a
// bare failure.
func TestCascadeReportsEveryFailure(t *testing.T) {
	c := NewSynthesizerCascade(nil,
		stubSynth{name: "openrouter", err: errors.New("429 rate limited")},
		stubSynth{name: "piper", err: errors.New("exec: not found")})

	_, err := c.Synthesize(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"openrouter", "429", "piper", "not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The key must never be required at config time, only at call time, and an
// absent key must be a clear refusal rather than a confusing 401.
func TestSynthesizeRefusesWithoutAKey(t *testing.T) {
	s := NewOpenRouterSynthesizer("https://openrouter.ai/api/v1/chat/completions", "openai/gpt-audio-mini", "marin", "", time.Second, nil)
	if ok, why := s.Available(); ok || !strings.Contains(why, "API key") {
		t.Errorf("Available() = %v, %q; want a refusal naming the missing key", ok, why)
	}
	if _, err := s.Synthesize(context.Background(), "hello"); err == nil {
		t.Error("expected a refusal without a key")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ io.Reader = strings.NewReader("")
