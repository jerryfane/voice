package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// descendants can be cleaned up with it. Without this, cancelling reaches
	// the direct child only: a launcher that backgrounds work and exits left
	// that descendant running after `voice doctor` returned, one orphan per
	// invocation. WaitDelay fixed the symptom - the probe returned on time,
	// because forcing the pipes closed unblocks the caller - while the process
	// it was waiting on stayed alive, which is why that fix looked complete.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}
	// Kept as a backstop: a descendant that calls setsid leaves the group
	// entirely and cannot be signalled through it, but the pipes still close
	// and the probe still returns. See killGroup for what that does and does
	// not guarantee.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	// Cleaned up here, not only in Cancel, because Cancel runs ONLY when the
	// context ends. The leak this fixes happens on the healthy path: the
	// launcher exits immediately, the deadline never fires, Wait returns once
	// WaitDelay closes the pipes, and the backgrounded descendant is still
	// running. Cancel alone would have left it exactly as it was.
	var groupErr error
	if cmd.Process != nil {
		groupErr = killGroup(cmd.Process.Pid)
	}
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

// killGroup terminates whatever remains of the probe's process group and
// VERIFIES the group is empty afterwards, rather than trusting the kill.
//
// kill(2) on a negative pid succeeds when at least ONE member was signalable,
// so a group holding a descendant that changed credentials - a setuid wrapper
// engine is reachable for a user-configured executable - reported complete
// success while that member kept running. Members are therefore enumerated
// from /proc, signalled INDIVIDUALLY, and the group re-read until empty.
//
// What this does NOT cover: a descendant that called setsid has left the group
// and survives the probe.
func killGroup(pgid int) error {
	return killGroupWith(pgid, func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) })
}

// killGroupWith takes the signal function so the survivor check can be tested
// for the case that matters: a signal that REPORTS SUCCESS while a member
// keeps running. Running as root every real kill lands, so a test using the
// real one cannot tell verification from blind trust.
func killGroupWith(pgid int, kill func(int) error) error {
	members, err := groupMembers(pgid)
	if err != nil {
		return err
	}
	// EVERY member is attempted, and errors are collected rather than
	// returned on the first failure. Returning early left later members
	// unsignalled: /proc enumeration order has nothing to do with which
	// members are killable, so one unreachable descendant could strand
	// perfectly killable siblings - leaving MORE processes alive than the
	// single member that was genuinely out of reach, while the error named
	// only that one.
	var signalErrs []error
	for _, m := range members {
		if kerr := kill(m.pid); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			signalErrs = append(signalErrs, fmt.Errorf("kill %d: %w", m.pid, kerr))
		}
	}
	// Two seconds, not 500ms: SIGKILL is not instantaneous for a process in
	// an uninterruptible wait or under heavy CPU starvation, and a deadline
	// tight enough to fire on scheduling delay produces a false survivor
	// report from the function that exists to prevent false reports.
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining, err := groupMembers(pgid)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return errors.Join(signalErrs...)
		}
		if time.Now().After(deadline) {
			// State is reported with each survivor. A process in D state has
			// been signalled and dies when its uninterruptible operation
			// finishes, so the reader can tell a pending kill from a member
			// that is genuinely beyond reach.
			survivors := make([]string, 0, len(remaining))
			pending := true
			for _, m := range remaining {
				survivors = append(survivors, fmt.Sprintf("%d(state %s)", m.pid, m.state))
				if m.state != "D" {
					pending = false
				}
			}
			why := "still has"
			if pending {
				why = "has a kill pending in uninterruptible sleep for"
			}
			signalErrs = append(signalErrs,
				fmt.Errorf("process group %d %s %d member(s): %s", pgid, why, len(remaining), strings.Join(survivors, " ")))
			return errors.Join(signalErrs...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// member is a process seen in a group, with the state that decides whether a
// survivor report means "beyond reach" or "dying".
type member struct {
	pid   int
	state string
}

// groupMembers reads /proc for processes whose process group is pgid,
// regardless of their credentials - a survivor that cannot be signalled is
// exactly the one a kill's return value hides.
//
// It FAILS rather than reporting zero members when /proc cannot be trusted to
// list them. os.ReadDir returns no error for a /proc that is not procfs, or
// one mounted with hidepid, where other users' entries are simply invisible;
// an empty listing would then look like an empty group and reproduce the very
// defect this function exists to close, through invisibility instead of a
// lying signal.
func groupMembers(pgid int) ([]member, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var members []member
	selfSeen := false
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a process directory
		}
		if pid == self {
			selfSeen = true
		}
		state, mpgid, ok := procStat(e.Name())
		if !ok {
			continue // exited between the listing and the read
		}
		// A zombie is listed with its group but cannot run and holds
		// nothing, so counting it would warn about a process already dead.
		if state == "Z" {
			continue
		}
		if mpgid == pgid {
			members = append(members, member{pid: pid, state: state})
		}
	}
	if !selfSeen {
		// Our own process is always in /proc on a working procfs. Not seeing
		// it means the listing cannot be relied on to reveal anyone else.
		return nil, fmt.Errorf("/proc did not list this process (%d), so group membership cannot be determined", self)
	}
	if hidden, reason := procHidesOthers(); hidden {
		return nil, fmt.Errorf("/proc hides other processes (%s), so group membership cannot be determined", reason)
	}
	return members, nil
}

// procHidesOthers reports whether this /proc mount can conceal other users'
// processes from this caller. Under hidepid, a non-root caller sees only its
// own, which makes an empty listing meaningless.
func procHidesOthers() (bool, string) {
	if os.Geteuid() == 0 {
		return false, "" // root sees every entry regardless of hidepid
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true, "cannot read /proc/self/mountinfo to check hidepid"
	}
	for _, line := range strings.Split(string(info), "\n") {
		if !strings.Contains(line, " proc ") && !strings.Contains(line, " proc\n") {
			continue
		}
		if !strings.Contains(line, " /proc ") {
			continue
		}
		for _, opt := range strings.Split(line, ",") {
			opt = strings.TrimSpace(opt)
			if strings.HasPrefix(opt, "hidepid=") && opt != "hidepid=0" && opt != "hidepid=off" {
				return true, opt
			}
		}
	}
	return false, ""
}

// procStat returns the state and process group from /proc/<name>/stat. comm
// can contain spaces, parentheses and newlines, so fields are counted from
// after the FINAL ')'.
func procStat(name string) (state string, pgid int, ok bool) {
	stat, err := os.ReadFile(filepath.Join("/proc", name, "stat"))
	if err != nil {
		return "", 0, false
	}
	closeParen := bytes.LastIndexByte(stat, ')')
	if closeParen < 0 {
		return "", 0, false
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) < 3 {
		return "", 0, false
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], pgid, true
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
