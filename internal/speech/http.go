package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

const maxTranscriptionResponse = 1 << 20

// Logger is the subset of log.Logger used by speech engines.
type Logger interface {
	Printf(string, ...any)
}

// HTTPDoer makes the remote engines testable without changing their behavior.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// OpenRouterTranscriber sends one accepted wake utterance to OpenRouter STT.
type OpenRouterTranscriber struct {
	Endpoint string
	Model    string
	APIKey   string
	Timeout  time.Duration
	Client   HTTPDoer
	Logger   Logger
}

func NewOpenRouterTranscriber(endpoint, model, apiKey string, timeout time.Duration, logger Logger) *OpenRouterTranscriber {
	return &OpenRouterTranscriber{Endpoint: endpoint, Model: model, APIKey: apiKey, Timeout: timeout, Client: http.DefaultClient, Logger: logger}
}

func (t *OpenRouterTranscriber) Name() string { return "openrouter/" + t.Model }
func (t *OpenRouterTranscriber) Available() (bool, string) {
	if strings.TrimSpace(t.APIKey) == "" {
		return false, "API key is not set"
	}
	if _, err := url.ParseRequestURI(t.Endpoint); err != nil {
		return false, "invalid endpoint: " + err.Error()
	}
	return true, t.Endpoint
}

func (t *OpenRouterTranscriber) Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error) {
	if ok, why := t.Available(); !ok {
		return "", errors.New(why)
	}
	body, contentType, err := transcriptionForm(audio.EncodeWAV(pcm, f), map[string]string{
		"model":           t.Model,
		"language":        "en",
		"response_format": "json",
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", contentType)
	started := time.Now()
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenRouter transcription: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptionResponse+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return "", fmt.Errorf("read OpenRouter transcription: %w", errors.Join(readErr, closeErr))
	}
	if len(raw) > maxTranscriptionResponse {
		return "", errors.New("OpenRouter transcription response exceeds 1 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenRouter transcription HTTP %d: %.300s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Text  string `json:"text"`
		Usage struct {
			Seconds float64 `json:"seconds"`
			Cost    float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode OpenRouter transcription: %w", err)
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", errors.New("OpenRouter returned an empty transcript")
	}
	if t.Logger != nil {
		t.Logger.Printf("stage=transcribe provider=openrouter model=%q state=ok ms=%.3f audio_seconds=%.3f cost_usd=%.8f", t.Model, elapsedMS(started), out.Usage.Seconds, out.Usage.Cost)
	}
	return text, nil
}

// WhisperServerTranscriber calls a loopback whisper.cpp server whose model is
// already resident in memory.
type WhisperServerTranscriber struct {
	Endpoint string
	Timeout  time.Duration
	Client   HTTPDoer
}

func NewWhisperServerTranscriber(endpoint string, timeout time.Duration) *WhisperServerTranscriber {
	return &WhisperServerTranscriber{Endpoint: endpoint, Timeout: timeout, Client: http.DefaultClient}
}

func (*WhisperServerTranscriber) Name() string { return "resident whisper.cpp" }
func (t *WhisperServerTranscriber) Available() (bool, string) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false, "invalid endpoint"
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "80")
	}
	conn, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err != nil {
		return false, err.Error()
	}
	if err := conn.Close(); err != nil {
		return false, err.Error()
	}
	return true, t.Endpoint
}

func (t *WhisperServerTranscriber) Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error) {
	body, contentType, err := transcriptionForm(audio.EncodeWAV(pcm, f), map[string]string{
		"language":        "en",
		"response_format": "json",
		"temperature":     "0.0",
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resident Whisper transcription: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptionResponse+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return "", fmt.Errorf("read resident Whisper transcription: %w", errors.Join(readErr, closeErr))
	}
	if len(raw) > maxTranscriptionResponse {
		return "", errors.New("resident Whisper response exceeds 1 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resident Whisper HTTP %d: %.300s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode resident Whisper transcription: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// Cascade tries transcribers in order. Errors and empty transcripts fall
// through; the final error names every failed engine without exposing secrets.
type Cascade struct {
	Engines []Transcriber
	Logger  Logger
}

func NewCascade(logger Logger, engines ...Transcriber) *Cascade {
	return &Cascade{Engines: engines, Logger: logger}
}

func (c *Cascade) Name() string {
	names := make([]string, 0, len(c.Engines))
	for _, e := range c.Engines {
		names = append(names, e.Name())
	}
	return strings.Join(names, " -> ")
}

func (c *Cascade) Available() (bool, string) {
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

func (c *Cascade) Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error) {
	var errs []error
	for i, e := range c.Engines {
		text, err := e.Transcribe(ctx, pcm, f)
		if err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
		if err == nil {
			err = errors.New("empty transcript")
		}
		errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		c.fallback(i, e, err)
	}
	return "", errors.Join(errs...)
}

func (c *Cascade) fallback(i int, e Transcriber, err error) {
	if c.Logger == nil || i+1 >= len(c.Engines) {
		return
	}
	c.Logger.Printf("stage=transcribe provider=%q state=fallback next=%q err=%v", e.Name(), c.Engines[i+1].Name(), err)
}

func transcriptionForm(wav []byte, fields map[string]string) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, "", err
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &body, w.FormDataContentType(), nil
}

func elapsedMS(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}
