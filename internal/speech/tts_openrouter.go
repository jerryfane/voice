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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

// openRouterAudioRate is the sample rate OpenRouter's audio models emit. The
// stream carries raw samples with no header, so the rate cannot be read from
// the payload and has to be known here.
const openRouterAudioRate = 24000

// maxSynthesisBytes bounds one response. A runaway stream would otherwise be
// buffered until the device ran out of memory, and this is a speakerphone
// answering in a sentence, not a podcast.
const maxSynthesisBytes = 16 << 20

// OpenRouterSynthesizer speaks through OpenRouter's audio-capable chat models.
//
// There is no text-to-speech endpoint to call. Of the models OpenRouter
// serves, only four emit audio and two of those generate music, so speech
// means asking a CHAT model to talk and collecting the audio it streams back.
// That shapes everything here:
//
//   - the request MUST stream. A plain request is refused outright with
//     "Audio output requires stream: true".
//   - streaming only supports raw pcm16; asking for wav is refused, so the
//     samples arrive headerless and this code frames them itself.
//   - the model is conversational, not a narrator, so the prompt constrains it
//     to read the line and the returned transcript is compared against what
//     was asked for.
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
		Endpoint: endpoint, Model: model, Voice: voice, APIKey: apiKey,
		Timeout: timeout, Client: http.DefaultClient, Logger: logger,
	}
}

func (s *OpenRouterSynthesizer) Name() string { return "openrouter/" + s.Model + " (" + s.Voice + ")" }

func (s *OpenRouterSynthesizer) Available() (bool, string) {
	if strings.TrimSpace(s.APIKey) == "" {
		return false, "API key is not set"
	}
	if strings.TrimSpace(s.Voice) == "" {
		return false, "no voice configured"
	}
	if _, err := url.ParseRequestURI(s.Endpoint); err != nil {
		return false, "invalid endpoint: " + err.Error()
	}
	return true, s.Endpoint + " as " + s.Voice
}

// speakPrompt constrains a conversational model to read one line. Without it
// the model answers the text instead of speaking it - asked to say "the shop
// closes at six", a chat model may reply "got it".
func speakPrompt(text string) string {
	return "Read the following text aloud exactly as written. " +
		"Do not answer it, do not comment on it, do not add or remove words. " +
		"Text to read:\n\n" + text
}

func (s *OpenRouterSynthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if ok, why := s.Available(); !ok {
		return nil, errors.New(why)
	}
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("nothing to speak")
	}

	body, err := json.Marshal(map[string]any{
		"model":      s.Model,
		"modalities": []string{"text", "audio"},
		"audio":      map[string]string{"voice": s.Voice, "format": "pcm16"},
		"stream":     true,
		"messages":   []map[string]string{{"role": "user", "content": speakPrompt(text)}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("speak: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The error body explains the refusal - a rejected voice name, an
		// unsupported format - and is far more useful than the status alone.
		detail, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptionResponse))
		closeErr := resp.Body.Close()
		return nil, fmt.Errorf("speak: %s: %s", resp.Status,
			strings.TrimSpace(string(detail))+joinErrs(readErr, closeErr))
	}

	pcm, transcript, streamErr := readAudioStream(resp.Body)
	closeErr := resp.Body.Close()
	if streamErr != nil || closeErr != nil {
		return nil, errors.Join(streamErr, closeErr)
	}
	if len(pcm) == 0 {
		return nil, errors.New("speak: the model returned no audio")
	}

	// The model may have said something other than what it was handed. That
	// is not a reason to stay silent - the audio is still speech the user
	// asked for - but it IS worth reporting, because an assistant that
	// improvises words it was not given is a correctness problem, and silent
	// drift is how it would go unnoticed.
	if s.Logger != nil && transcript != "" && !sameSpokenText(transcript, text) {
		s.Logger.Printf("speech drift: asked %q, model said %q", text, transcript)
	}

	return audio.EncodeWAV(bytesToSamples(pcm), audio.Format{SampleRate: openRouterAudioRate, Channels: 1}), nil
}

// joinErrs renders read/close trouble alongside a status message without
// letting either error be silently dropped.
func joinErrs(errs ...error) string {
	joined := errors.Join(errs...)
	if joined == nil {
		return ""
	}
	return " (" + joined.Error() + ")"
}

// readAudioStream collects base64 audio deltas out of an SSE response.
func readAudioStream(r io.Reader) ([]byte, string, error) {
	var (
		// Plain slices throughout: Buffer.Write and Builder.WriteByte both
		// return results this repo does not allow to be discarded, and
		// append says the same thing with nothing to ignore.
		pcm []byte
		// A slice, not a strings.Builder: Builder.WriteString returns a
		// result this repo does not allow to be discarded, and the
		// transcript is a handful of fragments, not a hot path.
		transcript []string
	)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
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
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// A malformed chunk is skipped rather than fatal: the rest of the
			// stream is still usable audio, and refusing to speak because one
			// frame did not parse would be worse than a slightly short reply.
			continue
		}
		if event.Error != nil && event.Error.Message != "" {
			return nil, "", fmt.Errorf("speak: %s", event.Error.Message)
		}
		for _, choice := range event.Choices {
			a := choice.Delta.Audio
			if a.Transcript != "" {
				transcript = append(transcript, a.Transcript)
			}
			if a.Data == "" {
				continue
			}
			chunk, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil {
				continue
			}
			if len(pcm)+len(chunk) > maxSynthesisBytes {
				return nil, "", fmt.Errorf("speak: audio exceeded %d bytes", maxSynthesisBytes)
			}
			pcm = append(pcm, chunk...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("speak: read stream: %w", err)
	}
	return pcm, strings.Join(transcript, ""), nil
}

// bytesToSamples reinterprets little-endian pcm16 as samples. An odd trailing
// byte is dropped: half a sample is not a sample.
func bytesToSamples(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return out
}

// sameSpokenText compares what was asked for with what was said, ignoring
// differences no listener would notice: case, surrounding space, and the
// punctuation a model adds or drops freely.
func sameSpokenText(a, b string) bool {
	return spokenKey(a) == spokenKey(b)
}

func spokenKey(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if space && len(out) > 0 {
				out = append(out, ' ')
			}
			space = false
			out = append(out, r)
		default:
			space = true
		}
	}
	return string(out)
}

// SynthesizerCascade speaks through the first engine that works.
//
// The remote voice is better; the local one is always there. A network blip
// must not leave the assistant mute, so a failure falls through to Piper
// rather than becoming silence.
type SynthesizerCascade struct {
	Engines []Synthesizer
	Logger  Logger
}

func NewSynthesizerCascade(logger Logger, engines ...Synthesizer) *SynthesizerCascade {
	return &SynthesizerCascade{Engines: engines, Logger: logger}
}

func (c *SynthesizerCascade) Name() string {
	names := make([]string, 0, len(c.Engines))
	for _, e := range c.Engines {
		names = append(names, e.Name())
	}
	return strings.Join(names, " -> ")
}

func (c *SynthesizerCascade) Available() (bool, string) {
	var details []string
	for _, e := range c.Engines {
		ok, why := e.Available()
		details = append(details, e.Name()+": "+why)
		if ok {
			return true, strings.Join(details, "; ")
		}
	}
	return false, strings.Join(details, "; ")
}

func (c *SynthesizerCascade) Synthesize(ctx context.Context, text string) ([]byte, error) {
	var errs []error
	for i, e := range c.Engines {
		wav, err := e.Synthesize(ctx, text)
		if err == nil && len(wav) > 0 {
			return wav, nil
		}
		if err == nil {
			err = errors.New("empty audio")
		}
		errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		if c.Logger != nil && i+1 < len(c.Engines) {
			c.Logger.Printf("speech engine %s failed (%v); falling back to %s", e.Name(), err, c.Engines[i+1].Name())
		}
	}
	return nil, errors.Join(errs...)
}
