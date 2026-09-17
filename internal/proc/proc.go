// Package proc runs the external programs voiced delegates to (audio capture,
// speech engines, the agent CLI) and expands the placeholders used in
// user-supplied command templates.
//
// Every externally configurable command in voiced is an argv slice, never a
// shell string. There is no shell interpolation anywhere in voiced, so a device
// name or a transcript containing quotes, semicolons or backticks can never
// become executable. Placeholders are substituted after argv splitting, which
// makes injection structurally impossible rather than merely filtered.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Expand replaces {key} placeholders in every argv element. A placeholder with
// no matching key is left untouched so typos surface loudly at runtime instead
// of silently vanishing.
func Expand(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		for k, v := range vars {
			a = strings.ReplaceAll(a, "{"+k+"}", v)
		}
		out[i] = a
	}
	return out
}

// Result carries the outcome of a completed command.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// Run executes argv with the given stdin, returning its output. A non-zero
// exit status is an error whose message includes trimmed stderr, because a
// silent engine failure is the single most confusing thing to debug in a voice
// pipeline.
func Run(ctx context.Context, argv []string, stdin []byte, timeout time.Duration) (Result, error) {
	if len(argv) == 0 {
		return Result{}, fmt.Errorf("empty command")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("%s timed out after %s", argv[0], timeout)
		}
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		if msg != "" {
			return res, fmt.Errorf("%s: %w: %s", argv[0], err, msg)
		}
		return res, fmt.Errorf("%s: %w", argv[0], err)
	}
	return res, nil
}

// Which reports whether a command exists on PATH, returning its resolved path.
func Which(name string) (string, bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return p, true
}
