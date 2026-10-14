package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"github.com/jerryfane/voice/internal/device"
)

// phoneAgentCommand deliberately bypasses the audio/session stack. Screen data
// and raw ADB output are tool results, not speech or structured-action receipts.
func phoneAgentCommand(ctx context.Context, devices *device.Registry, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: voice phone ID observe | voice phone ID adb COMMAND [ARGS...]")
	}
	d, err := devices.Get(args[0])
	if err != nil {
		return err
	}
	phone, ok := d.(*device.Android)
	if !ok {
		return fmt.Errorf("%s is not an Android phone", d.ID())
	}
	switch args[1] {
	case "observe":
		if len(args) != 2 {
			return errors.New("usage: voice phone ID observe (no extra arguments)")
		}
		observation, err := phone.Observe(ctx)
		if err != nil {
			return err
		}
		data, err := json.Marshal(observation)
		if err != nil {
			return fmt.Errorf("render Android observation: %w", err)
		}
		stdout.println(string(data))
		return stdout.err
	case "adb":
		out, err := phone.ADB(ctx, args[2:])
		// %s preserves binary output and exact newlines, including on failure.
		stdout.printf("%s", out.Stdout)
		stderr.printf("%s", out.Stderr)
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				// Stderr has already been emitted verbatim; don't duplicate its
				// private contents inside the CLI's generic error prefix.
				return fmt.Errorf("Android ADB command failed: %w", exit)
			}
			return err
		}
		return errors.Join(stdout.err, stderr.err)
	default:
		return errors.New("usage: voice phone ID observe | voice phone ID adb COMMAND [ARGS...]")
	}
}
