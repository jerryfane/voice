package speech

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A binary that cannot start must be reported as unusable. `voice doctor`
// called the speech engine OK on the strength of the file existing, while the
// service logged a loader failure once per utterance and transcribed nothing.
func TestRunnableRejectsABinaryThatCannotStart(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken-engine")
	// A shim that reproduces the loader's own failure and exit status.
	script := "#!/bin/sh\necho 'broken-engine: error while loading shared libraries: libwhisper.so.1: cannot open shared object file: No such file or directory' >&2\nexit 127\n"
	if err := os.WriteFile(broken, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, detail := runnable(broken)
	if ok {
		t.Fatalf("a binary that exits 127 on a loader failure was reported usable: %s", detail)
	}
	if !strings.Contains(detail, "libwhisper.so.1") {
		t.Errorf("detail does not say what went wrong: %q", detail)
	}

	working := filepath.Join(dir, "working-engine")
	if err := os.WriteFile(working, []byte("#!/bin/sh\necho 'usage: working-engine [options]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, detail := runnable(working); !ok {
		t.Errorf("a working engine was rejected: %s", detail)
	}

	// Engines disagree about --help's exit status; a non-zero exit with real
	// output must not be mistaken for a broken install.
	grumpy := filepath.Join(dir, "grumpy-engine")
	if err := os.WriteFile(grumpy, []byte("#!/bin/sh\necho 'usage: grumpy-engine'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, detail := runnable(grumpy); !ok {
		t.Errorf("an engine that exits 1 on --help was rejected: %s", detail)
	}
}

// The probe must leave nothing running. A launcher that backgrounds a
// descendant and exits used to leave that descendant alive after `voice
// doctor` returned - one orphan per invocation - and the earlier fix hid it:
// WaitDelay unblocked the caller, so the probe returned in about a second
// while the process it had been waiting on kept running to completion.
//
// This asserts the descendant is GONE, not that the probe was quick. A
// timing-only assertion is precisely what let the leak through review.
func TestRunnableLeavesNoDescendantRunning(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	launcher := filepath.Join(dir, "launcher-engine")
	// The shim records the pid it backgrounds, so the test can ask the
	// operating system about that exact process rather than about timing.
	script := "#!/bin/sh\nsleep 45 &\necho $! > " + pidFile + "\necho 'usage: launcher-engine'\nexit 0\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ok, detail := runnable(launcher)
	elapsed := time.Since(start)
	if !ok {
		t.Logf("engine reported unusable after %v: %s", elapsed, detail)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("shim never recorded a descendant pid, so this test proves nothing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("unusable descendant pid %q: %v", raw, err)
	}

	// Signal 0 asks whether the process exists without touching it. Give the
	// kill a moment to land; a descendant that survives sleeps for 45s, so a
	// two-second window cannot pass by luck.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return // gone, which is the whole assertion
			}
			t.Fatalf("cannot check descendant %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			// Do not leave it behind for the rest of the suite.
			if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil {
				t.Logf("cleanup kill of %d failed: %v", pid, kerr)
			}
			t.Fatalf("descendant %d is still running %v after the probe returned; WaitDelay only unblocks the caller, the process group must be killed", pid, elapsed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
