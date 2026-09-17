package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/proc"
)

// CommandTranscriber adapts any file-oriented STT executable.
type CommandTranscriber struct {
	Engine  string
	Argv    []string
	Model   string
	Timeout time.Duration
	Faults  faults.Reporter
}

// NewCommandTranscriber builds a transcriber. report receives failures that
// happen after the answer is already known - temp-file cleanup - which have no
// caller left to return them to.
func NewCommandTranscriber(name string, argv []string, model string, timeout time.Duration, report faults.Reporter) *CommandTranscriber {
	return &CommandTranscriber{Engine: name, Argv: argv, Model: model, Timeout: timeout, Faults: report}
}

// report forwards a failure that has no caller left to receive it.
func (t *CommandTranscriber) report(err error) {
	if t.Faults != nil {
		t.Faults.Report(err)
	}
}
func (t *CommandTranscriber) Name() string { return t.Engine }
func (t *CommandTranscriber) Available() (bool, string) {
	if len(t.Argv) == 0 {
		return false, "command is empty"
	}
	p, ok := proc.Which(t.Argv[0])
	if !ok {
		return false, t.Argv[0] + " not found on PATH"
	}
	if t.Model != "" {
		if _, err := os.Stat(t.Model); err != nil {
			return false, fmt.Sprintf("model %s: %v", t.Model, err)
		}
	}
	return true, p
}
func (t *CommandTranscriber) Transcribe(ctx context.Context, pcm []int16, f audio.Format) (string, error) {
	tmp, err := os.CreateTemp("", "voice-*.wav")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer func() {
		if err := os.Remove(name); err != nil {
			t.report(fmt.Errorf("removing the capture temp file %s: %w", name, err))
		}
	}()
	if _, err = tmp.Write(audio.EncodeWAV(pcm, f)); err != nil {
		return "", errors.Join(err, tmp.Close())
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	argv := proc.Expand(t.Argv, map[string]string{"file": name, "model": t.Model})
	stdin := []byte(nil)
	if !contains(t.Argv, "{file}") {
		stdin = audio.EncodeWAV(pcm, f)
	}
	res, err := proc.Run(ctx, argv, stdin, t.Timeout)
	if err != nil {
		return "", err
	}
	return cleanTranscript(string(res.Stdout), name), nil
}

// CommandSynthesizer adapts a TTS executable that writes WAV to stdout or to
// the path named by {out}. Text is supplied on stdin unless {text} is present.
type CommandSynthesizer struct {
	Engine  string
	Argv    []string
	Model   string
	Timeout time.Duration
	Faults  faults.Reporter
}

// NewCommandSynthesizer builds a synthesizer. report receives temp-file
// cleanup failures, as above.
func NewCommandSynthesizer(name string, argv []string, model string, timeout time.Duration, report faults.Reporter) *CommandSynthesizer {
	return &CommandSynthesizer{Engine: name, Argv: argv, Model: model, Timeout: timeout, Faults: report}
}
func (s *CommandSynthesizer) report(err error) {
	if s.Faults != nil {
		s.Faults.Report(err)
	}
}
func (s *CommandSynthesizer) Name() string { return s.Engine }
func (s *CommandSynthesizer) Available() (bool, string) {
	if len(s.Argv) == 0 {
		return false, "command is empty"
	}
	p, ok := proc.Which(s.Argv[0])
	if !ok {
		return false, s.Argv[0] + " not found on PATH"
	}
	if s.Model != "" {
		if _, err := os.Stat(s.Model); err != nil {
			return false, fmt.Sprintf("model %s: %v", s.Model, err)
		}
	}
	return true, p
}
func (s *CommandSynthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	out, err := os.CreateTemp("", "voice-tts-*.wav")
	if err != nil {
		return nil, err
	}
	outName := out.Name()
	// The engine writes this file itself, so Voice only needed the name; a
	// failure to close the empty placeholder still means a leaked descriptor.
	if err := out.Close(); err != nil {
		return nil, err
	}
	defer func() {
		if err := os.Remove(outName); err != nil {
			s.report(fmt.Errorf("removing the synthesis temp file %s: %w", outName, err))
		}
	}()
	vars := map[string]string{"model": s.Model, "text": text, "out": outName}
	argv := proc.Expand(s.Argv, vars)
	stdin := []byte(nil)
	if !contains(s.Argv, "{text}") {
		stdin = []byte(text + "\n")
	}
	res, err := proc.Run(ctx, argv, stdin, s.Timeout)
	if err != nil {
		return nil, err
	}
	if contains(s.Argv, "{out}") {
		b, e := os.ReadFile(outName)
		if e != nil {
			return nil, e
		}
		return b, nil
	}
	if !bytes.HasPrefix(res.Stdout, []byte("RIFF")) {
		return nil, fmt.Errorf("%s returned %d bytes, not RIFF/WAVE audio", s.Engine, len(res.Stdout))
	}
	return res.Stdout, nil
}
func contains(argv []string, needle string) bool {
	for _, a := range argv {
		if strings.Contains(a, needle) {
			return true
		}
	}
	return false
}
func cleanTranscript(s, file string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	base := filepath.Base(file)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, base) || strings.HasPrefix(line, "whisper_") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.Contains(line, "]") {
			if i := strings.Index(line, "]"); i >= 0 {
				line = strings.TrimSpace(line[i+1:])
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}
