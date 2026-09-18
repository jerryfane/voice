package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// The agent's own workspace must always be searched, whoever runs doctor.
//
// `voice doctor` is normally run by the operator - root, or the pi user -
// while the agent writes as voice-agent, and the sudoers grant the installer
// writes permits only voice-agent-run, so doctor cannot re-run itself as the
// agent. Resolving only the caller's home meant doctor looked in
// /root/voice-workspace while requests accumulated in
// /home/voice-agent/voice-workspace: recorded, and reported as none. Worse
// than the refusal it replaced, because it would have claimed to record them.
//
// My own smoke test passed only because it forced HOME. The review caught it.
func TestRequestsPathsAlwaysIncludeTheAgentWorkspace(t *testing.T) {
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", "")
	t.Setenv("HOME", t.TempDir())

	// Stand in for the account, so this asserts the behaviour on any host
	// rather than skipping everywhere but a real device.
	agentDir := t.TempDir()
	restore := agentHome
	agentHome = func() (string, bool) { return agentDir, true }
	t.Cleanup(func() { agentHome = restore })

	want := filepath.Join(agentDir, "voice-workspace", "REQUESTS.tsv")
	if got := requestsPaths(); !hasPath(got, want) {
		t.Errorf("paths = %v, missing the agent workspace %q; requests would sit where doctor never looks", got, want)
	}
}

// Both sides of the relocation must be searched, because the reader and the
// writer do not see the same environment: the wrapper runs under `sudo -n -H`,
// which strips VOICE_AGENT_WORKSPACE from the service environment. If doctor
// trusted one answer it could report "no requests" while a request sat in the
// other location - the silent wrong answer this feature exists to end.
func TestRequestsPathsCoverBothSidesOfARelocation(t *testing.T) {
	relocated := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", relocated)
	t.Setenv("HOME", t.TempDir())

	restore := agentHome
	agentHome = func() (string, bool) { return agentDir, true }
	t.Cleanup(func() { agentHome = restore })

	got := requestsPaths()
	for _, want := range []string{
		filepath.Join(relocated, "REQUESTS.tsv"),
		filepath.Join(agentDir, "voice-workspace", "REQUESTS.tsv"),
	} {
		if !hasPath(got, want) {
			t.Errorf("paths = %v, missing %q", got, want)
		}
	}
}

// An explicit override is the operator naming a file outright, so it must be
// searched first - and searched, not obeyed exclusively, since the writer may
// not have seen the same variable.
func TestRequestsPathsPutAnExplicitOverrideFirst(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "elsewhere.tsv")
	t.Setenv("VOICE_REQUESTS", explicit)
	t.Setenv("VOICE_AGENT_WORKSPACE", "")
	t.Setenv("HOME", t.TempDir())

	got := requestsPaths()
	if len(got) == 0 || got[0] != explicit {
		t.Errorf("paths = %v, want %q first", got, explicit)
	}
}

// Duplicates would make doctor print the same request twice and read the same
// file repeatedly; on a default install every candidate resolves alike.
func TestRequestsPathsAreDeduplicated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", filepath.Join(home, "voice-workspace"))
	t.Setenv("HOME", home)

	restore := agentHome
	agentHome = func() (string, bool) { return home, true }
	t.Cleanup(func() { agentHome = restore })

	got := requestsPaths()
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("paths = %v, contains %q twice", got, p)
		}
		seen[p] = true
	}
	if len(got) != 1 {
		t.Errorf("paths = %v; one workspace must collapse to one path", got)
	}
}

// With nothing resolvable at all, a relative path is still better than an
// invented absolute one: naming /home/voice-agent on a host without that
// account points at a file nothing writes to.
func TestRequestsPathsNeverEmpty(t *testing.T) {
	t.Setenv("VOICE_REQUESTS", "")
	t.Setenv("VOICE_AGENT_WORKSPACE", "")
	t.Setenv("HOME", "")

	restore := agentHome
	agentHome = func() (string, bool) { return "", false }
	t.Cleanup(func() { agentHome = restore })

	got := requestsPaths()
	if len(got) == 0 {
		t.Fatal("no candidate paths at all; doctor would never look anywhere")
	}
	for _, p := range got {
		if strings.Contains(p, "voice-agent") {
			t.Errorf("paths = %v, invented an agent path with no such account", got)
		}
	}
}
