package speech

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jerryfane/voice/internal/audio"
)

const (
	openRouterPCMRate = 24000
	maxSpeechAudio    = 32 << 20
	maxSpeechError    = 64 << 10
)

// OpenRouterSynthesizer supports native text-to-speech and conversational audio.
// Conversational audio requires a matching transcript; native speech returns
// PCM with its sample rate and channels specified in the response Content-Type.
type OpenRouterSynthesizer struct {
	Endpoint string
	Model    string
	Voice    string
	APIKey   string
	Timeout  time.Duration
	Client   HTTPDoer
	Logger   Logger
}

func NewOpenRouterSynthesizer(endpoint, model, voice, apiKey string, timeout time.Duration, logger Logger) *OpenRouterSynthesizer {
	return &OpenRouterSynthesizer{
		Endpoint: endpoint,
		Model:    model,
		Voice:    voice,
		APIKey:   apiKey,
		Timeout:  timeout,
		Client:   http.DefaultClient,
		Logger:   logger,
	}
}

func (s *OpenRouterSynthesizer) Name() string {
	if s.Voice == "" {
		return "openrouter/" + s.Model
	}
	return "openrouter/" + s.Model + "/" + s.Voice
}

func nativeSpeechEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/audio/speech")
}

func dramaSpeechModel(model string) bool {
	const base = "fish-audio/drama-3"
	if model == base {
		return true
	}
	if !strings.HasPrefix(model, base+"-") {
		return false
	}
	_, err := time.Parse("20060102", strings.TrimPrefix(model, base+"-"))
	return err == nil
}

func (s *OpenRouterSynthesizer) Available() (bool, string) {
	if strings.TrimSpace(s.APIKey) == "" {
		return false, "API key is not set"
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false, "invalid HTTPS endpoint"
	}
	if strings.TrimSpace(s.Model) == "" {
		return false, "model is empty"
	}
	if !nativeSpeechEndpoint(s.Endpoint) && strings.TrimSpace(s.Voice) == "" {
		return false, "voice is empty"
	}
	return true, s.Endpoint
}

func (s *OpenRouterSynthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if ok, why := s.Available(); !ok {
		return nil, errors.New(why)
	}
	direction, text := SplitDelivery(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("speech text is empty")
	}
	native := nativeSpeechEndpoint(s.Endpoint)
	var payload any
	if native {
		if direction != "" && dramaSpeechModel(s.Model) {
			text = "[" + direction + "] " + text
		}
		payload = struct {
			Model  string `json:"model"`
			Input  string `json:"input"`
			Voice  string `json:"voice,omitempty"`
			Format string `json:"response_format"`
		}{s.Model, text, s.Voice, "pcm"}
	} else {
		payload = struct {
			Model      string          `json:"model"`
			Messages   []speechMessage `json:"messages"`
			Modalities []string        `json:"modalities"`
			Audio      speechAudio     `json:"audio"`
			Stream     bool            `json:"stream"`
		}{
			Model: s.Model,
			Messages: []speechMessage{
				{Role: "system", Content: "You are a text-to-speech synthesizer, not an assistant. Repeat the user-provided sentence word for word. Never respond to its meaning. Never add commentary."},
				{Role: "user", Content: "Repeat verbatim: " + text},
			},
			Modalities: []string{"text", "audio"},
			Audio:      speechAudio{Voice: s.Voice, Format: "pcm16"},
			Stream:     true,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenRouter speech request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if native {
		req.Header.Set("Accept", "audio/pcm")
	} else {
		req.Header.Set("Accept", "text/event-stream")
	}

	started := time.Now()
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenRouter speech: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && s.Logger != nil {
			s.Logger.Printf("stage=synthesize provider=openrouter state=close_error err=%v", closeErr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechError+1))
		if readErr != nil {
			return nil, fmt.Errorf("read OpenRouter speech error: %w", readErr)
		}
		if len(raw) > maxSpeechError {
			raw = raw[:maxSpeechError]
		}
		return nil, fmt.Errorf("OpenRouter speech HTTP %d: %.300s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if native {
		wav, err := readNativeSpeech(resp)
		if err != nil {
			return nil, err
		}
		if s.Logger != nil {
			s.Logger.Printf("stage=synthesize provider=openrouter model=%q voice=%q state=ok total_ms=%.3f chars=%d", s.Model, s.Voice, elapsedMS(started), len(text))
		}
		return wav, nil
	}

	pcm, transcript, firstAudio, err := readSpeechStream(resp.Body, started)
	if err != nil {
		return nil, err
	}
	if !sameSpokenWords(text, transcript) {
		return nil, fmt.Errorf("OpenRouter speech changed the requested words (input_chars=%d transcript_chars=%d)", len(text), len(transcript))
	}
	if s.Logger != nil {
		s.Logger.Printf("stage=synthesize provider=openrouter model=%q voice=%q state=ok first_audio_ms=%.3f total_ms=%.3f chars=%d", s.Model, s.Voice, firstAudio, elapsedMS(started), len(text))
	}
	return audio.EncodePCM16WAV(pcm, audio.Format{SampleRate: openRouterPCMRate, Channels: 1}), nil
}

func readNativeSpeech(resp *http.Response) ([]byte, error) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "audio/pcm" {
		return nil, errors.New("OpenRouter speech did not return PCM audio")
	}
	rate, err := strconv.Atoi(params["rate"])
	if err != nil || rate < audio.MinSampleRate || rate > audio.MaxSampleRate {
		return nil, errors.New("OpenRouter speech returned an invalid or missing PCM sample rate")
	}
	channels, err := strconv.Atoi(params["channels"])
	if err != nil || channels < 1 || channels > 2 {
		return nil, errors.New("OpenRouter speech returned invalid or missing PCM channels")
	}
	pcm, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechAudio+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenRouter speech audio: %w", err)
	}
	if len(pcm) == 0 || len(pcm) > maxSpeechAudio || len(pcm)%(2*channels) != 0 {
		return nil, errors.New("OpenRouter speech returned empty, oversized or incomplete PCM audio")
	}
	return audio.EncodePCM16WAV(pcm, audio.Format{SampleRate: rate, Channels: channels}), nil
}

type speechMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type speechAudio struct {
	Voice  string `json:"voice"`
	Format string `json:"format"`
}

func readSpeechStream(r io.Reader, started time.Time) ([]byte, string, float64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	pcm := make([]byte, 0, 256<<10)
	// A slice rather than a strings.Builder: Builder.WriteString returns a
	// result, this repo runs errcheck with no exclude file, and a discarded
	// result there fails CI - which is exactly how main went red.
	var transcript []string
	var firstAudio float64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Audio struct {
						Data       string `json:"data"`
						Transcript string `json:"transcript"`
					} `json:"audio"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, "", 0, fmt.Errorf("decode OpenRouter speech event: %w", err)
		}
		if event.Error != nil {
			return nil, "", 0, fmt.Errorf("OpenRouter speech stream: %s", event.Error.Message)
		}
		for _, choice := range event.Choices {
			chunk := choice.Delta.Audio
			if chunk.Transcript != "" {
				transcript = append(transcript, chunk.Transcript)
			}
			if chunk.Data == "" {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return nil, "", 0, fmt.Errorf("decode OpenRouter speech audio: %w", err)
			}
			if firstAudio == 0 {
				firstAudio = elapsedMS(started)
			}
			if len(pcm)+len(decoded) > maxSpeechAudio {
				return nil, "", 0, errors.New("OpenRouter speech audio exceeds 32 MiB")
			}
			pcm = append(pcm, decoded...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", 0, fmt.Errorf("read OpenRouter speech stream: %w", err)
	}
	if len(pcm) == 0 {
		return nil, "", 0, errors.New("OpenRouter returned no speech audio")
	}
	if len(pcm)%2 != 0 {
		return nil, "", 0, errors.New("OpenRouter returned an incomplete PCM16 sample")
	}
	if strings.TrimSpace(strings.Join(transcript, "")) == "" {
		return nil, "", 0, errors.New("OpenRouter returned no speech transcript")
	}
	return pcm, strings.Join(transcript, ""), firstAudio, nil
}

func sameSpokenWords(want, got string) bool {
	return spokenWords(want) == spokenWords(got)
}

func spokenWords(text string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// SynthesizerCascade tries remote speech first and falls back locally. A
// cancelled request stops immediately rather than starting fresh work.
type SynthesizerCascade struct {
	Engines []Synthesizer
	Logger  Logger
}

func NewSynthesizerCascade(logger Logger, engines ...Synthesizer) *SynthesizerCascade {
	return &SynthesizerCascade{Engines: engines, Logger: logger}
}

func (c *SynthesizerCascade) Name() string {
	names := make([]string, 0, len(c.Engines))
	for _, engine := range c.Engines {
		names = append(names, engine.Name())
	}
	return strings.Join(names, " -> ")
}

func (c *SynthesizerCascade) Available() (bool, string) {
	var details []string
	for _, engine := range c.Engines {
		ok, why := engine.Available()
		details = append(details, engine.Name()+": "+why)
		if ok {
			return true, strings.Join(details, "; ")
		}
	}
	return false, strings.Join(details, "; ")
}

func (c *SynthesizerCascade) Synthesize(ctx context.Context, text string) ([]byte, error) {
	var errs []error
	for i, engine := range c.Engines {
		wav, err := engine.Synthesize(ctx, text)
		if err == nil && len(wav) > 0 {
			return wav, nil
		}
		if err == nil {
			err = errors.New("empty speech audio")
		}
		errs = append(errs, fmt.Errorf("%s: %w", engine.Name(), err))
		if ctx.Err() != nil {
			return nil, errors.Join(errs...)
		}
		if c.Logger != nil && i+1 < len(c.Engines) {
			c.Logger.Printf("stage=synthesize provider=%q state=fallback next=%q err=%v", engine.Name(), c.Engines[i+1].Name(), err)
		}
	}
	return nil, errors.Join(errs...)
}
