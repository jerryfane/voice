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
	"strconv"
	"strings"
	"syscall"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/proc"
	"github.com/jerryfane/voice/internal/session"
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
	cfg, p, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load config: %w (run `voice init`)", err)
	}
	_ = p
	a := session.Build(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "doctor":
		return doctor(ctx, cfg, a)
	case "devices":
		return listDevices(ctx, a)
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
	case "light":
		return commandDevice(ctx, a, "light", args[1:])
	case "tv":
		return commandDevice(ctx, a, "tv", args[1:])
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
  devices                   list configured devices and live state
  light ID OP [VALUE]       control a Magic Home light
  tv ID OP                  control an HDMI-CEC TV
  config                    print effective config
  version                   print version

Light ops: on, off, toggle, color NAME, white 0-100, brightness 0-100
TV ops: on, off, volume-up, volume-down, mute
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
func doctor(ctx context.Context, c config.Config, a *session.Assistant) error {
	bad := 0
	check := func(label string, ok bool, detail string) {
		status := "OK"
		if !ok {
			status = "FAIL"
			bad++
		}
		stdout.printf("%-12s %-4s %s\n", label, status, detail)
	}
	_, ok := proc.Which(c.Input.Command[0])
	check("microphone", ok, a.Recorder.Describe())
	_, ok = proc.Which(c.Output.Command[0])
	check("speaker", ok, a.Player.Describe())
	ok, d := a.STT.Available()
	check("speech-to-text", ok, a.STT.Name()+": "+d)
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
