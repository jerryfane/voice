package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// requestsPath must resolve the AGENT's workspace, not the caller's.
//
// `voice doctor` is normally run by the operator - root, or the pi user -
// while the agent writes as voice-agent, and the sudoers grant the installer
// writes permits only voice-agent-run, so doctor cannot re-run itself as the
// agent. Deriving the path from the caller's home meant doctor looked in
// /root/voice-workspace while requests accumulated in
// /home/voice-agent/voice-workspace: recorded, and reported as none. Worse
// than the refusal it replaced, because it would have claimed to record them.
//
// My own smoke test passed only because it forced HOME. The review caught it.
func TestRequestsPathResolvesTheAgentWorkspaceNotTheCallers(t *testing.T) {
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", "")
	callerHome := t.TempDir()
	t.Setenv("HOME", callerHome)

	// Stand in for the account, so this asserts the behaviour on any host
	// rather than skipping everywhere but a real device.
	agentDir := t.TempDir()
	restore := agentHome
	agentHome = func() (string, bool) { return agentDir, true }
	t.Cleanup(func() { agentHome = restore })

	got := requestsPath()
	if want := filepath.Join(agentDir, "voice-workspace", "REQUESTS.tsv"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, callerHome) {
		t.Errorf("path %q is under the caller's home; requests would be recorded where doctor never looks", got)
	}
}

// With no such account - a developer machine rather than the device - falling
// back to the caller's own workspace is correct: inventing /home/voice-agent
// would report a path nothing writes to.
func TestRequestsPathFallsBackWhenTheAccountIsAbsent(t *testing.T) {
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	restore := agentHome
	agentHome = func() (string, bool) { return "", false }
	t.Cleanup(func() { agentHome = restore })

	if got, want := requestsPath(), filepath.Join(home, "voice-workspace", "REQUESTS.tsv"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// The overrides exist so a non-standard install can point both halves at one
// file, and they must win over the account lookup - otherwise an operator who
// relocated the workspace gets a reader that looks elsewhere.
func TestRequestsPathHonoursExplicitOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("VOICE_AGENT_WORKSPACE", dir)
	t.Setenv("VOICE_REQUESTS", "")
	if got, want := requestsPath(), filepath.Join(dir, "REQUESTS.tsv"); got != want {
		t.Errorf("with VOICE_AGENT_WORKSPACE set, path = %q, want %q", got, want)
	}

	explicit := filepath.Join(dir, "elsewhere.tsv")
	t.Setenv("VOICE_REQUESTS", explicit)
	if got := requestsPath(); got != explicit {
		t.Errorf("VOICE_REQUESTS must win outright: path = %q, want %q", got, explicit)
	}
}
