package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jerryfane/herdr-voice/internal/config"
	"github.com/jerryfane/herdr-voice/internal/device"
	"github.com/jerryfane/herdr-voice/internal/proc"
	"github.com/jerryfane/herdr-voice/internal/session"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "voiced:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	fs := flag.NewFlagSet("voiced", flag.ContinueOnError)
	path := fs.String("config", "", "config path (default: $VOICED_CONFIG or ~/.config/voiced/config.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return usage()
	}
	if args[0] == "version" {
		fmt.Println("voiced", version)
		return nil
	}
	if args[0] == "init" {
		force := len(args) > 1 && args[1] == "--force"
		return initConfig(*path, force)
	}
	cfg, p, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load config: %w (run `voiced init`)", err)
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
		fmt.Fprintf(os.Stderr, "Listening for %q. Ctrl-C to stop.\n", cfg.Wake.Phrases[0])
		return a.Run(ctx)
	case "say":
		if len(args) < 2 {
			return errors.New("usage: voiced say TEXT")
		}
		return a.Speak(ctx, strings.Join(args[1:], " "))
	case "ask":
		if len(args) < 2 {
			return errors.New("usage: voiced ask TEXT")
		}
		p, err := a.HandleText(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println(p.Speak)
		if p.Speak != "" {
			return a.Speak(ctx, p.Speak)
		}
		return nil
	case "light":
		return commandDevice(ctx, a, "light", args[1:])
	case "tv":
		return commandDevice(ctx, a, "tv", args[1:])
	case "config":
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return nil
	default:
		return usage()
	}
}
func usage() error {
	fmt.Print(`voiced — Herdr Voice local assistant runtime

Usage: voiced [--config PATH] COMMAND

  init [--force]           write a complete starter config
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
	fmt.Println("Wrote", path)
	fmt.Println("Run `voiced doctor`, then edit device names and addresses.")
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
		fmt.Printf("%-12s %-4s %s\n", label, status, detail)
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
	if c.Input.TelephonyHID != "" {
		f, e := os.OpenFile(c.Input.TelephonyHID, os.O_WRONLY, 0)
		if e == nil {
			f.Close()
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
		fmt.Printf("%-12s %-6s %-40s state=%v", d.ID(), d.Kind(), d.Describe(), s)
		if e != nil {
			fmt.Printf(" error=%v", e)
		}
		fmt.Println()
	}
	return nil
}
func commandDevice(ctx context.Context, a *session.Assistant, kind string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: voiced %s ID OP [VALUE]", kind)
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
	b, _ := json.Marshal(s)
	fmt.Println(string(b))
	return nil
}
