package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/proc"
	"github.com/jerryfane/voice/internal/requests"
	"github.com/jerryfane/voice/internal/session"
	"github.com/jerryfane/voice/internal/vad"
	"github.com/jerryfane/voice/internal/wake"
)

var version = "0.1.0-dev"

// stdout and stderr remember the first write failure instead of discarding it,
// so a command whose output went nowhere exits non-zero rather than reporting
// success for work nobody received. Every write goes through them, so one
// place decides what a failed write means and no site drops the error.
//
// What this does NOT catch, stated because the first version of this comment
// claimed it did: a closed pipe. Go's runtime raises SIGPIPE for writes to
// file descriptors 1 and 2 and lets the default disposition kill the process,
// which is what `voice version | true` does - exit 141, no error to report and
// nothing to report it to. That is correct Unix behaviour for a CLI and is
// deliberately not overridden here. What the printer catches is the rest: a
// full disk, an I/O error, a redirect to a file that cannot be written.
var (
	stdout = &printer{w: os.Stdout}
	stderr = &printer{w: os.Stderr}
)

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, a...)
}

func (p *printer) println(a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintln(p.w, a...)
}

func (p *printer) print(a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprint(p.w, a...)
}

// agentAccount is the restricted account the voice agent runs as, matching
// packaging/voice.service and packaging/voice-agent-run.
const agentAccount = "voice-agent"

// agentHome resolves that account's home directory. It is a variable so the
// resolution can be tested on a host that has no such account: without a
// seam, the test for "look in the AGENT's home, not the caller's" skips
// everywhere except a real device, which is another check that cannot fail.
// Production uses the real lookup below and nothing flips it.
var agentHome = func() (string, bool) {
	u, err := user.Lookup(agentAccount)
	if err != nil || u.HomeDir == "" {
		return "", false
	}
	return u.HomeDir, true
}

// requestCandidate is one place the journal could be, and whether a failure
// to read it is a fault or merely worth mentioning.
type requestCandidate struct {
	path     string
	required bool
}

// requestsPaths lists every place the voice agent's journal could be, most
// specific first, deduplicated by file identity.
//
// It returns a LIST rather than a single answer on purpose. The reader and the
// writer resolve this path in different processes with different environments:
// the wrapper runs under `sudo -n -H`, which strips VOICE_AGENT_WORKSPACE out
// of the service environment, so a workspace relocated in voice.service is
// honoured by one side and invisible to the other. Picking one candidate means
// doctor can report "no requests" while a real request sits in the other
// location - a silent wrong answer, which is the defect this whole feature
// exists to end. So doctor reads them all and says which file it found.
func requestsPaths() []requestCandidate {
	var paths []requestCandidate
	add := func(p string, required bool) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		// Deduplicate by file IDENTITY, not by string. A relocated
		// workspace is often a symlink or a bind mount of the agent's own,
		// so two different strings name one file: counting it twice would
		// report two requests where one was spoken and print it twice, and
		// a reader that invents requests is no more trustworthy than one
		// that hides them.
		info, err := os.Stat(p)
		for _, existing := range paths {
			if existing.path == p {
				return
			}
			if err != nil {
				continue
			}
			other, otherErr := os.Stat(existing.path)
			if otherErr == nil && os.SameFile(info, other) {
				return
			}
		}
		paths = append(paths, requestCandidate{path: p, required: required})
	}

	// Required candidates are the places the journal is MEANT to be, named
	// by an operator or by the service account. The caller's own home is a
	// backstop, so a stray unreadable file there must not fail a device
	// whose real journal is healthy.
	add(os.Getenv("VOICE_REQUESTS"), true)
	if ws := os.Getenv("VOICE_AGENT_WORKSPACE"); ws != "" {
		add(filepath.Join(ws, "REQUESTS.tsv"), true)
	}
	if home, ok := agentHome(); ok {
		add(filepath.Join(home, "voice-workspace", "REQUESTS.tsv"), true)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, "voice-workspace", "REQUESTS.tsv"), false)
	}
	if len(paths) == 0 {
		add(filepath.Join("voice-workspace", "REQUESTS.tsv"), false)
	}
	return paths
}

func main() {
	err := run(os.Args[1:])
	// A failed write is a failure of the command, not a detail: report it if
	// nothing worse happened first. Piped-and-closed output never reaches
	// here - SIGPIPE has already ended the process - so this is the full-disk
	// and bad-redirect path.
	if err == nil {
		err = errors.Join(stdout.err, stderr.err)
	}
	if err != nil {
		stderr.println("voice:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	fs := flag.NewFlagSet("voice", flag.ContinueOnError)
	path := fs.String("config", "", "config path (default: $VOICE_CONFIG or ~/.config/voice/config.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return usage()
	}
	if args[0] == "version" {
		stdout.println("voice", version)
		return nil
	}
	if args[0] == "init" {
		force := len(args) > 1 && args[1] == "--force"
		return initConfig(*path, force)
	}
	if args[0] == "on" || args[0] == "off" || args[0] == "status" {
		return controlService(args[0])
	}
	cfg, loaded, err := config.Load(*path)
	if err != nil {
		// No `voice init` suggestion bolted on here: when nothing exists
		// anywhere, config.NotFoundError already names the files it looked
		// for and what init would write. Appending the advice
		// unconditionally is how it came to be offered for a config that
		// existed and was simply somewhere else.
		return fmt.Errorf("load config: %w", err)
	}
	a := session.Build(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "doctor":
		return doctor(ctx, cfg, a, loaded)
	case "devices":
		return listDevices(ctx, a)
	case "wake":
		if len(args) < 2 {
			return errors.New("usage: voice wake TRANSCRIPT")
		}
		return checkWake(cfg, strings.Join(args[1:], " "))
	case "listen":
		stderr.printf("Listening for %q. Ctrl-C to stop.\n", cfg.Wake.Phrases[0])
		return a.Run(ctx)
	case "say":
		if len(args) < 2 {
			return errors.New("usage: voice say TEXT")
		}
		return a.Speak(ctx, strings.Join(args[1:], " "))
	case "ask":
		if len(args) < 2 {
			return errors.New("usage: voice ask TEXT")
		}
		p, err := a.HandleText(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		stdout.println(p.Speak)
		if p.Speak != "" {
			return a.Speak(ctx, p.Speak)
		}
		return nil
	case "execute":
		if len(args) != 2 {
			return errors.New("usage: voice execute PLAN_JSON")
		}
		var p brain.Plan
		if err := json.Unmarshal([]byte(args[1]), &p); err != nil {
			return fmt.Errorf("decode plan: %w", err)
		}
		if err := a.ApplyActions(ctx, p.Actions); err != nil {
			return err
		}
		if p.Speak == "" {
			return nil
		}
		stdout.println(p.Speak)
		return a.Speak(ctx, p.Speak)
	case "light":
		return commandDevice(ctx, a, "light", args[1:])
	case "tv":
		return commandDevice(ctx, a, "tv", args[1:])
	case "music":
		return commandDevice(ctx, a, "music", args[1:])
	case "proposals":
		return proposalsCommand(a, cfg, loaded, args[1:])
	case "config":
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("rendering the effective config: %w", err)
		}
		stdout.println(string(b))
		return nil
	default:
		return usage()
	}
}
func usage() error {
	stdout.print(`voice — persistent local voice agent

Usage: voice [--config PATH] COMMAND

  init [--force]           write a complete starter config
  on                        start listening
  off                       stop listening
  status                    report whether Voice is listening
  doctor                    check audio, engines and devices
  listen                    run the wake-word voice loop
  ask TEXT                  process one text command through the same brain
  say TEXT                  synthesize and play speech
  execute PLAN_JSON         execute structured actions and spoken response
  devices                   list configured devices and live state
  wake TRANSCRIPT           report whether those words would wake Voice
  light ID OP [VALUE]       control a Magic Home light
  tv ID OP                  control an HDMI-CEC TV
  music ID OP [TYPE QUERY]  control a configured music player
  proposals CMD [ARGS]      track what Voice was asked for and could not do
  config                    print effective config
  version                   print version

Light ops: on, off, toggle, color NAME, white 0-100, brightness 0-100
TV ops: on, off, volume-up, volume-down, mute
Music ops: play [track|album|artist|playlist QUERY], resume, pause, next, previous, volume 0-10, volume-up, volume-down
Proposals: list [--json], show ID, due [--json], record REQUEST [--title T] [--scope S] [--risks R],
           notified ID, approve ID, decline ID [REASON], issue ID NUMBER, implementing ID AGENT,
           complete ID, fail ID DETAIL, expire
`)
	return nil
}

func controlService(action string) error {
	if action == "status" {
		err := exec.Command("systemctl", "is-active", "--quiet", "voice.service").Run()
		if err == nil {
			stdout.println("Voice is on.")
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			stdout.println("Voice is off.")
			return nil
		}
		return fmt.Errorf("query voice.service: %w", err)
	}
	verb := map[string]string{"on": "start", "off": "stop"}[action]
	argv := []string{"systemctl", verb, "voice.service"}
	if os.Geteuid() != 0 {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s voice.service: %w: %s", verb, err, strings.TrimSpace(string(out)))
	}
	stdout.printf("Voice is %s.\n", map[string]string{"on": "on", "off": "off"}[action])
	return nil
}

// checkWake answers whether a spoken transcript would wake the assistant,
// using wake.Match - the SAME function the listener calls, not a reimagining
// of it, because a checker with its own copy of the rules can agree with
// itself while disagreeing with the device.
//
// It exists because "the config file contains the phrase" is not the question
// anyone actually has. After an install, the question is whether the words
// somebody says out loud are accepted, and until now the only way to answer
// that was to say them and see.
func checkWake(c config.Config, transcript string) error {
	matched, command, phrase := wake.Match(transcript, c.Wake.Phrases, c.Wake.Fuzz)
	if !matched {
		stdout.printf("%-4s %q would NOT wake Voice; configured phrases: %v\n", "NO", transcript, c.Wake.Phrases)
		return errWakeNoMatch
	}
	if command == "" {
		stdout.printf("%-4s %q wakes Voice on %q, with no trailing command\n", "YES", transcript, phrase)
		return nil
	}
	stdout.printf("%-4s %q wakes Voice on %q, command %q\n", "YES", transcript, phrase, command)
	return nil
}

// errWakeNoMatch makes a non-match a failing exit status so an installer or a
// deploy receipt can assert it, while staying distinguishable from a real
// error like an unreadable config.
var errWakeNoMatch = errors.New("no wake match")

func initConfig(path string, force bool) error {
	if path == "" {
		var err error
		path, err = config.Path()
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s exists; use init --force to replace it", path)
	}
	c := config.Default()
	if err := config.Save(c, path); err != nil {
		return err
	}
	stdout.println("Wrote", path)
	stdout.println("Run `voice doctor`, then edit device names and addresses.")
	return nil
}
func doctor(ctx context.Context, c config.Config, a *session.Assistant, loaded string) error {
	bad := 0
	check := func(label string, ok bool, detail string) {
		status := "OK"
		if !ok {
			status = "FAIL"
			bad++
		}
		stdout.printf("%-12s %-4s %s\n", label, status, detail)
	}
	// Unconditionally, including on success: "which config am I actually
	// running" was unanswerable, and a diagnostic that reports on a file it
	// does not name is how someone ends up tuning the wrong one.
	check("config", true, loaded)
	_, ok := proc.Which(c.Input.Command[0])
	check("microphone", ok, a.Recorder.Describe())
	_, ok = proc.Which(c.Output.Command[0])
	check("speaker", ok, a.Player.Describe())
	ok, d := a.STT.Available()
	check("speech-to-text", ok, a.STT.Name()+": "+d)
	if gate, managed := a.VAD.(vad.ManagedSegmenter); managed {
		ok, d = gate.Available()
		check("wake gate", ok, gate.Name()+": "+d)
	} else if a.WakeSTT != nil {
		ok, d = a.WakeSTT.Available()
		check("wake gate", ok, a.WakeSTT.Name()+": "+d)
	}
	ok, d = a.TTS.Available()
	check("text-to-speech", ok, a.TTS.Name()+": "+d)
	ok, d = a.Brain.Available()
	check("brain", ok, a.Brain.Name()+": "+d)
	stdout.printf("%-12s %-4s %s\n", "wake feedback", "INFO", a.Feedback.Describe())
	// The wake line reports how each phrase will be matched, because the
	// tolerance silently does nothing for a phrase with too little sound in
	// it. The sentence is built in the wake package beside the rule it
	// describes: assembling it here produced a self-contradiction, telling an
	// exact-only single-word phrase that it would wake on sound-alikes.
	for _, phrase := range c.Wake.Phrases {
		stdout.printf("%-12s %-4s %q: %s\n", "wake phrase", "INFO", phrase,
			wake.DescribeMatching(phrase, c.Wake.Fuzz))
	}
	// Requests the voice agent recorded because it could not act on them
	// itself. Reported here because the alternative is what already
	// happened: a spoken request refused politely, written nowhere, and
	// found days later only because someone thought to ask.
	candidates := requestsPaths()
	found := map[string][]requests.Request{}
	total := 0
	ignored := 0
	for _, candidate := range candidates {
		journal, err := requests.Load(candidate.path)
		if err != nil {
			if candidate.required {
				check("requests", false, err.Error())
			} else {
				// A leftover file in whoever-ran-doctor's own home is not
				// the device's problem, but it is not nothing either: say
				// it and carry on rather than failing a healthy install or
				// swallowing it silently.
				stdout.printf("%-12s %-4s ignoring an unreadable backstop: %v\n", "requests", "INFO", err)
			}
			continue
		}
		if len(journal.Requests) > 0 {
			found[candidate.path] = journal.Requests
			total += len(journal.Requests)
		}
		ignored += journal.Ignored
	}
	if total > 0 {
		stdout.printf("%-12s %-4s %d spoken request(s) need someone with code access:\n", "requests", "INFO", total)
		for _, candidate := range candidates {
			reqs, ok := found[candidate.path]
			if !ok {
				continue
			}
			// Name the file whenever more than one location holds
			// requests, so a relocated workspace is visible rather than
			// looking like one merged list.
			if len(found) > 1 {
				stdout.printf("%-12s      in %s:\n", "", candidate.path)
			}
			for _, r := range reqs {
				// A line the loader could not parse is marked, not shown
				// as if it were a clean entry: the writer is an agent
				// following prose instructions, so a mangled line is
				// likely and reading it as dated-and-fine would misreport
				// what was actually recorded.
				when := "unparsed"
				if !r.When.IsZero() {
					when = r.When.Local().Format("2 Jan 15:04")
				}
				stdout.printf("%-12s      %-13s %s\n", "", when, r.Text)
			}
		}
	}
	if ignored > 0 {
		// Said out loud rather than quietly subtracted. The installed
		// journal held two lines whose text was the agent's own launch
		// command and doctor read them back as spoken feature requests; the
		// fix must not swap that wrong answer for an unexplained gap
		// between what the file holds and what this reports.
		stdout.printf("%-12s %-4s %d line(s) ignored: launcher or plumbing text, not anything a person asked for\n",
			"requests", "INFO", ignored)
	}
	proposalsDoctor(a, c, check)
	if c.Timers.Enabled {
		if a.Timers == nil {
			check("timers", false, "enabled but no scheduler was built")
		} else {
			check("timers", true, fmt.Sprintf("%d running, %d expired while offline",
				len(a.Timers.List()), len(a.Missed)))
		}
	}
	if c.Input.TelephonyHID != "" {
		f, e := os.OpenFile(c.Input.TelephonyHID, os.O_WRONLY, 0)
		if e == nil {
			e = f.Close()
		}
		check("telephony HID", e == nil, c.Input.TelephonyHID+": "+errText(e))
	}
	for _, dev := range a.Devices.List() {
		state, e := dev.Read(ctx)
		check(dev.Kind()+" "+dev.ID(), e == nil, fmt.Sprintf("%s state=%v err=%v", dev.Describe(), state, e))
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}
func errText(e error) string {
	if e == nil {
		return "writable"
	}
	return e.Error()
}
func listDevices(ctx context.Context, a *session.Assistant) error {
	for _, d := range a.Devices.List() {
		s, e := d.Read(ctx)
		stdout.printf("%-12s %-6s %-40s state=%v", d.ID(), d.Kind(), d.Describe(), s)
		if e != nil {
			stdout.printf(" error=%v", e)
		}
		stdout.println()
	}
	return nil
}
func commandDevice(ctx context.Context, a *session.Assistant, kind string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: voice %s ID OP [VALUE]", kind)
	}
	d, err := a.Devices.Get(args[0])
	if err != nil {
		return err
	}
	if d.Kind() != kind {
		return fmt.Errorf("%s is a %s, not a %s", d.ID(), d.Kind(), kind)
	}
	op := strings.ReplaceAll(args[1], "-", "_")
	cmd := device.Command{Op: device.Op(op)}
	if cmd.Op == device.OpColor {
		if len(args) < 3 {
			return errors.New("color requires a name")
		}
		cmd.Args = map[string]any{"name": args[2]}
	}
	if cmd.Op == device.OpWhite || cmd.Op == device.OpBrightness {
		if len(args) < 3 {
			return errors.New("level requires 0-100")
		}
		n, e := strconv.Atoi(args[2])
		if e != nil {
			return e
		}
		cmd.Args = map[string]any{"level": n}
	}
	if cmd.Op == device.OpVolume {
		if len(args) < 3 {
			return errors.New("music volume requires a level from 0 to 10")
		}
		n, e := strconv.Atoi(args[2])
		if e != nil {
			return e
		}
		cmd.Args = map[string]any{"level": n}
	}
	if kind == "music" && cmd.Op == device.OpPlay && len(args) > 2 {
		mediaType := "track"
		queryAt := 2
		switch args[2] {
		case "track", "album", "artist", "playlist":
			mediaType = args[2]
			queryAt = 3
		}
		if queryAt >= len(args) {
			return fmt.Errorf("%s requires a query", mediaType)
		}
		cmd.Args = map[string]any{
			"query": strings.Join(args[queryAt:], " "),
			"type":  mediaType,
		}
	}
	if !device.Supports(d, cmd.Op) {
		return fmt.Errorf("%s does not support %s", d.ID(), cmd.Op)
	}
	s, err := d.Apply(ctx, cmd)
	if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("rendering device state: %w", err)
	}
	stdout.println(string(b))
	return nil
}
