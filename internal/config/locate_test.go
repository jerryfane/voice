package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two configs with DIFFERENT wake phrases, so the test can prove which file
// was loaded rather than merely that loading succeeded. A single config would
// pass whichever path the code picked.
func writeConfigWithPhrase(t *testing.T, path, phrase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Wake.Phrases = []string{phrase}
	if err := Save(c, path); err != nil {
		t.Fatal(err)
	}
}

// An installed device keeps its config at SystemPath and the service passes it
// explicitly, so every command a person types to inspect that device used to
// fail - and the failure advised `voice init`, which writes a SECOND config
// the service never reads. Someone following that advice would tune a file
// with no effect on the running assistant and conclude it worked.
func TestLocateFallsBackToTheInstalledConfig(t *testing.T) {
	// A caller with no config of their own, which is the operator's position
	// on a device: root, or the service account.
	t.Setenv("VOICE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	system := filepath.Join(t.TempDir(), "etc-voice.json")
	writeConfigWithPhrase(t, system, "installed phrase")
	restore := systemPath
	systemPath = system
	t.Cleanup(func() { systemPath = restore })

	got, err := Locate()
	if err != nil {
		t.Fatalf("Locate() failed while an installed config exists: %v", err)
	}
	if got != system {
		t.Errorf("Locate() = %q, want the installed config %q", got, system)
	}

	// And the whole point: loading with no explicit path must read it.
	cfg, loaded, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") failed: %v", err)
	}
	if loaded != system {
		t.Errorf("Load reported %q, want %q", loaded, system)
	}
	if cfg.Wake.Phrases[0] != "installed phrase" {
		t.Errorf("loaded phrases %v; a different file than the installed config was read", cfg.Wake.Phrases)
	}
}

// A developer's own config must still win, or a system install silently
// shadows the file they are editing.
func TestLocatePrefersTheCallersOwnConfig(t *testing.T) {
	t.Setenv("VOICE_CONFIG", "")
	own := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", own)
	t.Setenv("HOME", t.TempDir())
	writeConfigWithPhrase(t, filepath.Join(own, "voice", "config.json"), "my own phrase")

	system := filepath.Join(t.TempDir(), "etc-voice.json")
	writeConfigWithPhrase(t, system, "installed phrase")
	restore := systemPath
	systemPath = system
	t.Cleanup(func() { systemPath = restore })

	cfg, loaded, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wake.Phrases[0] != "my own phrase" {
		t.Errorf("loaded %v from %q; the caller's own config must win", cfg.Wake.Phrases, loaded)
	}
}

// An explicit VOICE_CONFIG is someone naming a file outright and must beat
// both, existing or not - otherwise a typo silently loads a different device's
// configuration.
func TestLocateHonoursAnExplicitOverrideEvenWhenAbsent(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "named.json")
	t.Setenv("VOICE_CONFIG", explicit)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	system := filepath.Join(t.TempDir(), "etc-voice.json")
	writeConfigWithPhrase(t, system, "installed phrase")
	restore := systemPath
	systemPath = system
	t.Cleanup(func() { systemPath = restore })

	got, err := Locate()
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Errorf("Locate() = %q, want the explicitly named %q", got, explicit)
	}
}

// With nothing anywhere, the error must name both files it considered. The old
// message appended "run `voice init`" to every load failure, including one for
// a config that existed and was simply elsewhere.
func TestLocateErrorNamesEveryFileItLookedFor(t *testing.T) {
	t.Setenv("VOICE_CONFIG", "")
	own := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", own)
	t.Setenv("HOME", t.TempDir())

	restore := systemPath
	systemPath = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { systemPath = restore })

	_, err := Locate()
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Locate() error = %v, want a NotFoundError", err)
	}
	msg := err.Error()
	for _, want := range []string{filepath.Join(own, "voice", "config.json"), systemPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}
