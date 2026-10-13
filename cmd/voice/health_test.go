package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/health"
)

func TestListenerHealthUsesRuntimeNotPersistentState(t *testing.T) {
	t.Setenv("RUNTIME_DIRECTORY", "/run/voice")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("XDG_STATE_HOME", "/do-not-write-state")
	if got := listenerHealthPath(); got != serviceHealthPath {
		t.Fatal(got)
	}
	t.Setenv("RUNTIME_DIRECTORY", "")
	if got := listenerHealthPath(); got != "/run/user/1000/voice/listener-health.json" {
		t.Fatal(got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := listenerHealthPath(); got != filepath.Join(os.TempDir(), "voice-health-"+strconv.Itoa(os.Geteuid()), "listener-health.json") {
		t.Fatal(got)
	}
}

func TestExplicitHealthReadNeedsNoServiceOrConfig(t *testing.T) {
	m := health.New()
	m.Capture([]int16{1})
	m.Delivered()
	m.LoopProgress()
	m.WakeProgress()
	m.WakeState("ready")
	m.SetWakeGate(func() string { return "candidate_hold" })
	path := filepath.Join(t.TempDir(), "health.json")
	publisher, err := health.Publish(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Error(err)
		}
	})
	var output bytes.Buffer
	original := stdout
	stdout = &printer{w: &output}
	t.Cleanup(func() { stdout = original })
	if err := healthCommand([]string{"--file", path, "--json"}); err != nil {
		t.Fatal(err)
	}
	var s health.Snapshot
	if err := json.Unmarshal(output.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.PID != os.Getpid() || s.WakeGate != "candidate_hold" {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestHealthCommandReportsMissingInsteadOfClaimingOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if err := healthCommand([]string{"--file", path}); err == nil {
		t.Fatalf("missing health: %v", err)
	}
	for _, args := range [][]string{{"--file"}, {"--other"}} {
		if err := healthCommand(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestHealthCommandReturnsFailureForProvenStall(t *testing.T) {
	m := health.New()
	ctx, done := m.Begin(context.Background(), "transcribe", -time.Second)
	defer done(ctx.Err())
	path := filepath.Join(t.TempDir(), "health.json")
	publisher, err := health.Publish(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Error(err)
		}
	})
	var output bytes.Buffer
	original := stdout
	stdout = &printer{w: &output}
	t.Cleanup(func() { stdout = original })
	if err := healthCommand([]string{"--file", path, "--json"}); err == nil {
		t.Fatal("overdue stage reported success")
	}
	var snapshot health.Snapshot
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Problem || len(snapshot.Stages) != 1 || !snapshot.Stages[0].Overdue {
		t.Fatalf("overdue diagnostic lost: %+v", snapshot)
	}
}
