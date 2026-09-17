package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	return runnable(p)
}

// runnable reports whether an engine binary can actually start. Checking that
// a file exists on PATH is not the same question: a binary built against
// shared libraries that were never installed passes every static check and
// then fails at exec time, once per utterance, transcribing nothing. That
// shipped, and `voice doctor` called it OK.
//
// Only a loader failure is treated as fatal. Engines disagree about the exit
// status of --help, so a non-zero exit with sensible output is not evidence of
// anything.
func runnable(path string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--help")
	// The probe runs in its own process group so a wrapper engine's
	// descendants can be killed with it. Without this, cancelling reaches the
	// direct child only: a launcher that backgrounds work and exits left that
	// descendant running after `voice doctor` returned, one orphan per
	// invocation. WaitDelay fixed the symptom - the probe returned on time,
	// because forcing the pipes closed unblocks the caller - while the process
	// it was waiting on stayed alive, which is why the fix looked complete.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel therefore signals the GROUP, negative pid, rather than the child.
	// Kill, not terminate: this is a --help probe with no state to flush, and a
	// wrapper that ignores SIGTERM is exactly the case that leaked.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// ESRCH means it exited first, which is the common, healthy path.
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// Kept as a backstop: if a descendant escapes the group by calling
	// setsid itself, the pipes still close and the probe still returns.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	// The group is killed here, not only in Cancel, because Cancel runs ONLY
	// when the context ends. The leak this fixes happens on the healthy path:
	// the launcher exits immediately, the deadline never fires, Wait returns
	// once WaitDelay closes the pipes, and the backgrounded descendant is
	// still running. Cancel alone would have left it exactly as it was.
	groupErr := killGroup(cmd.Process)
	text := strings.TrimSpace(string(out))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false, path + ": did not respond to --help within 5s"
	}
	if strings.Contains(text, "error while loading shared libraries") {
		return false, path + ": " + firstLine(text)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 127 {
		return false, fmt.Sprintf("%s: exit 127, cannot start: %s", path, firstLine(text))
	}
	if err != nil && text == "" {
		return false, fmt.Sprintf("%s: %v", path, err)
	}
	if groupErr != nil {
		// The engine starts, so it is usable; but a probe that cannot clean up
		// after itself leaves processes on the host once per doctor run, and
		// staying silent about it is how the first leak survived a fix.
		return true, fmt.Sprintf("%s (warning: probe process group may have survived: %v)", path, groupErr)
	}
	return true, path
}

// killGroup signals the probe's whole process group. A wrapper engine that
// backgrounds a descendant and exits is the reachable case: the descendant
// inherits the group, so one signal reaches it without the caller needing to
// know it exists.
func killGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		// Nothing left in the group: the common, healthy outcome.
		return nil
	default:
		return err
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
	return runnable(p)
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
