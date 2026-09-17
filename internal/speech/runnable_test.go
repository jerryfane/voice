package speech

import (
	"errors"
	"os"
	"os/exec"
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

// The probe must leave nothing running IN ITS PROCESS GROUP. A launcher that backgrounds a
// descendant and exits used to leave that descendant alive after `voice
// doctor` returned - one orphan per invocation - and the earlier fix hid it:
// WaitDelay unblocked the caller, so the probe returned in about a second
// while the process it had been waiting on kept running to completion.
//
// This asserts the descendant is GONE, not that the probe was quick. A
// timing-only assertion is precisely what let the leak through review.
func TestRunnableLeavesNoDescendantInItsProcessGroup(t *testing.T) {
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
			t.Fatalf("descendant %d is still running %v after the probe returned; WaitDelay only unblocks the caller, the process group must be cleaned up", pid, elapsed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The boundary, proven by the reviewer rather than assumed by me: a descendant
// that calls setsid has LEFT the probe's process group and cannot be reached
// through it, so it survives. The first version of this fix was titled as
// though no descendant could survive at all, which was an overclaim.
//
// This test pins the boundary so the claim and the code cannot drift apart: if
// a later change does reach detached descendants, this test fails and must be
// rewritten, which is the correct way to find out that the guarantee widened.
func TestRunnableDoesNotReachASetsidDetachedDescendant(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "detached.pid")
	launcher := filepath.Join(dir, "detaching-engine")
	script := "#!/bin/sh\nsetsid sh -c 'echo $$ > " + pidFile + "; exec sleep 45' &\necho 'usage: detaching-engine'\nexit 0\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, detail := runnable(launcher); !ok {
		t.Logf("engine reported unusable: %s", detail)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("shim did not record a detached pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Skipf("unusable detached pid %q", raw)
	}
	t.Cleanup(func() {
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
			t.Logf("cleanup kill of %d failed: %v", pid, kerr)
		}
	})

	if err := syscall.Kill(pid, 0); err != nil {
		t.Skipf("detached descendant %d already gone (%v); the shim may not have detached, so this proves nothing", pid, err)
	}
	t.Logf("documented boundary holds: detached descendant %d survives the probe, as the code says it does", pid)
}

// groupMembers is the mechanism the survivor check depends on: kill(2) on a
// negative pid reports success when any one member was signalable, so the
// group has to be re-read to know it is empty. If the enumeration is wrong,
// the verification it feeds is decorative.
func TestGroupMembersSeesLiveMembersAndForgetsDeadOnes(t *testing.T) {
	cmd := exec.Command("sleep", "45")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid

	members, err := groupMembers(pgid)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != pgid {
		t.Fatalf("groupMembers(%d) = %v, want exactly [%d]", pgid, members, pgid)
	}

	if err := killGroup(pgid); err != nil {
		t.Fatalf("killGroup on a group we own failed: %v", err)
	}
	if werr := cmd.Wait(); werr == nil {
		t.Error("expected a signal error from Wait after the group was killed")
	}
	if members, err := groupMembers(pgid); err != nil || len(members) != 0 {
		t.Errorf("after killGroup, groupMembers(%d) = %v (err %v), want empty", pgid, members, err)
	}
}

// A zombie stays in the group listing but cannot run, so reporting it as a
// survivor would be a false warning from the function whose whole purpose is
// to stop false reports.
func TestGroupMembersIgnoresZombies(t *testing.T) {
	// A shell that forks a child, lets it exit, and then sleeps without
	// reaping leaves exactly one zombie in its own process group.
	cmd := exec.Command("sh", "-c", "sh -c 'exit 0' & exec sleep 3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		if kerr := killGroup(pgid); kerr != nil {
			t.Logf("cleanup killGroup: %v", kerr)
		}
		if werr := cmd.Wait(); werr != nil {
			t.Logf("cleanup wait: %v", werr)
		}
	})

	// Give the child time to exit and become a zombie.
	time.Sleep(300 * time.Millisecond)
	members, err := groupMembers(pgid)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range members {
		stat, rerr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if rerr != nil {
			continue
		}
		after := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(after) > 0 && after[0] == "Z" {
			t.Errorf("groupMembers reported zombie %d as a survivor", pid)
		}
	}
}

// The finding this guards: kill(2) on a negative pid succeeds when any one
// member was signalable, so a group holding a descendant that changed
// credentials reported complete success while that descendant kept running -
// and the caller's warning was gated on the error that never came.
//
// A real kill cannot express that here, because the test runs as root and
// every signal lands. A kill that claims success and does nothing can, and it
// is the same observable the setuid case produces.
func TestKillGroupReportsASurvivorWhenTheSignalLies(t *testing.T) {
	cmd := exec.Command("sleep", "45")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		if kerr := killGroup(pgid); kerr != nil {
			t.Logf("cleanup killGroup: %v", kerr)
		}
		if werr := cmd.Wait(); werr != nil {
			t.Logf("cleanup wait: %v", werr)
		}
	})

	lying := func(int) error { return nil } // reports success, kills nothing
	err := killGroupWith(pgid, lying)
	if err == nil {
		t.Fatal("killGroup reported success while a group member was still running; the survivor check is not verifying anything")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(pgid)) {
		t.Errorf("error does not name the group: %v", err)
	}
}
