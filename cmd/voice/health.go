package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/health"
	"github.com/jerryfane/voice/internal/session"
)

const serviceHealthPath = "/run/voice/listener-health.json"

func listenerHealthPath() string {
	if runtime := os.Getenv("RUNTIME_DIRECTORY"); runtime != "" {
		return filepath.Join(strings.Split(runtime, ":")[0], "listener-health.json")
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		return filepath.Join(runtime, "voice", "listener-health.json")
	}
	return filepath.Join(os.TempDir(), "voice-health-"+strconv.Itoa(os.Geteuid()), "listener-health.json")
}

func startListenerHealth(ctx context.Context, a *session.Assistant, c config.Config) (func() error, error) {
	path := listenerHealthPath()
	a.Health = health.New()
	// Each cascade attempt has its own timeout. The total request deadline
	// prevents an unexpected implementation from working forever, while keeping
	// every configured attempt's allowance (including process cleanup).
	b := session.HealthBudgets{Transcribe: c.STT.Timeout.D(), Synthesize: c.TTS.Timeout.D(), Plan: c.Brain.Timeout.D()}
	if v := c.STT.Resident; v != nil {
		b.Transcribe += v.Timeout.D()
	}
	if v := c.STT.OpenRouter; v != nil {
		b.Transcribe += v.Timeout.D()
	}
	if v := c.TTS.OpenRouter; v != nil {
		b.Synthesize += v.Timeout.D()
	}
	if v := c.Brain.Jev; v != nil {
		b.Plan += v.Timeout.D()
	}
	if v := c.Brain.Front; v != nil {
		b.Front = v.Timeout.D()
	}
	b.Transcribe += 5 * time.Second
	b.Synthesize += 5 * time.Second
	b.Plan += 5 * time.Second
	a.HealthBudgets = b
	publisher, err := health.Publish(ctx, path, a.Health)
	if err != nil {
		return nil, fmt.Errorf("publish listener health: %w", err)
	}
	if a.Logger != nil {
		a.Logger.Printf("listener health snapshot=%q", path)
	}
	return publisher.Close, nil
}

// Both commands bypass session.Build: inspecting health must never open the
// live microphone or touch its HID reports. --file selects a foreground listener
// explicitly; a service read never falls back to another account's state.
func healthCommand(args []string) error       { return listenerHealthCommand(args) }
func statusHealthCommand(args []string) error { return listenerHealthCommand(args) }

func listenerHealthCommand(args []string) error {
	path, asJSON, explicit := serviceHealthPath, false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--file":
			i++
			if i == len(args) || args[i] == "" {
				return errors.New("usage: voice health|status [--json] [--file PATH]")
			}
			path, explicit = args[i], true
		default:
			return errors.New("usage: voice health|status [--json] [--file PATH]")
		}
	}
	pid := 0
	if !explicit {
		var err error
		pid, err = serviceListenerPID()
		if err != nil {
			return err
		}
	}
	s, err := health.Read(path, time.Now(), pid)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			command := "sudo -n -u voice-agent /usr/local/bin/voice health"
			if explicit {
				command += " --file '" + strings.ReplaceAll(path, "'", "'\\''") + "'"
			}
			return fmt.Errorf("listener health unverified: cannot read private snapshot %s; run %s (add --json if needed)", path, command)
		}
		return fmt.Errorf("listener health unverified at %s: %w", path, err)
	}
	if asJSON {
		encoded, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		stdout.println(string(encoded))
	} else {
		stdout.printf("Listener PID %d; snapshot age %s; %s\n", s.PID, time.Since(s.Updated).Round(time.Millisecond), path)
		stdout.printf("Capture: %s; frames=%d; latest peak=%d/32768; last frame=%s; last nonzero=%s; discontinuities=%d\n", s.CaptureState, s.Frames, s.Peak, age(s.LastFrame), age(s.LastSignal), s.Discontinuities)
		stdout.printf("Wake: %s; gate=%s; processing=%s; last local wake=%s; event loop=%s\n", s.WakeState, s.WakeGate, age(s.LastWakeProgress), age(s.LastWake), age(s.LastLoop))
		for _, stage := range s.Stages {
			stdout.printf("Stage: %s; age=%s; deadline=%s; overdue=%v\n", stage.Name, time.Duration(stage.AgeMS)*time.Millisecond, stage.Deadline.Format(time.RFC3339), stage.Overdue)
		}
		if !s.LastFailure.At.IsZero() {
			stdout.printf("Last failure: %s (%s), %s\n", s.LastFailure.Stage, s.LastFailure.Kind, age(s.LastFailure.At))
		}
		stdout.println("Assessment:", s.Diagnosis)
		stdout.println("Stream activity is not proof of audible input or wake recognition; half-duplex guards intentionally suppress detection during playback.")
		if s.WakeGate == "candidate_hold" {
			stdout.println("Feedback hold: speech or above-threshold noise is being considered; sustained noise can suppress thinking pulses without a transport failure.")
		}
	}
	if s.Problem {
		return errors.New("live listener reports a problem (see snapshot)")
	}
	return nil
}

func age(at time.Time) string {
	if at.IsZero() {
		return "not observed"
	}
	return time.Since(at).Round(time.Millisecond).String() + " ago"
}

func serviceListenerPID() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "voice.service", "--property=MainPID", "--value").Output()
	if err != nil {
		return 0, fmt.Errorf("service state unverified: %w; use voice health --file PATH for a foreground listener", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, errors.New("service MainPID is invalid; listener health unverified")
	}
	if pid <= 0 {
		return 0, errors.New("Voice service has no running main process; listener is not verified")
	}
	return pid, nil
}
