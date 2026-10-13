package health

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublisherPrivateIdentityLockAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	p, err := Publish(context.Background(), path, New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	s, err := Read(path, time.Now(), os.Getpid())
	if err != nil || s.PID != os.Getpid() {
		t.Fatalf("read: %+v %v", s, err)
	}
	if _, err := Read(path, time.Now(), os.Getpid()+1); err == nil {
		t.Fatal("wrong MainPID accepted")
	}
	if second, err := Publish(context.Background(), path, New()); err == nil {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("second publisher accepted")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot retained: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock inode must remain: %v", err)
	}
	p2, err := Publish(context.Background(), path, New())
	if err != nil {
		t.Fatal("lock not released:", err)
	}
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadRejectsStaleIdentityAndUnsafeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	boot, start, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	base := New().Snapshot(time.Now())
	base.PID, base.BootID, base.ProcessStart = os.Getpid(), boot, start
	cases := []struct {
		name   string
		mutate func(*Snapshot)
		want   string
	}{
		{"stale", func(s *Snapshot) { s.Updated = time.Now().Add(-2 * FreshFor) }, "stale"},
		{"future", func(s *Snapshot) { s.Updated = time.Now().Add(time.Hour) }, "stale"},
		{"reused PID", func(s *Snapshot) { s.ProcessStart = "1" }, "identity changed"},
		{"different boot", func(s *Snapshot) { s.BootID = "old-boot" }, "identity changed"},
		{"invalid PID", func(s *Snapshot) { s.PID = -1 }, "identity unverified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutate(&s)
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path, time.Now(), 0); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read: %v", err)
			}
		})
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, time.Now(), 0); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("public snapshot: %v", err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, time.Now(), 0); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestPublisherRejectsSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if p, err := Publish(context.Background(), filepath.Join(dir, "health.json"), New()); err == nil {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("shared directory accepted")
	}
}

func TestAtomicUpdateReplacesWholeSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	m := New()
	p, err := Publish(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	})
	m.Capture([]int16{123})
	if err := p.update(); err != nil {
		t.Fatal(err)
	}
	var previous Snapshot
	if err := json.NewDecoder(old).Decode(&previous); err != nil {
		t.Fatal(err)
	}
	current, err := Read(path, time.Now(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if previous.Frames != 0 || current.Frames != 1 {
		t.Fatalf("not atomic: previous=%d current=%d", previous.Frames, current.Frames)
	}
}
