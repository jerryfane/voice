package speech

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
