// Package requests records things the user asked for out loud that the voice
// agent could not do itself.
//
// It exists because a spoken request vanished. The owner asked the device to
// make the thinking sound's volume configurable; the agent answered "I can't
// adjust the thinking sound volume from the controls available to me", wrote
// nothing, and that was the end of it. The refusal was correct - it runs as a
// restricted account with no checkout of this repository - but it left the
// request nowhere, and it only surfaced days later because the owner asked
// whether anyone had looked.
//
// So the agent appends here instead. No new privileges: the file lives in the
// workspace it already owns, and `voice doctor` reads it back.
package requests

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Request is one thing asked for aloud and not yet acted on.
type Request struct {
	// When the request was spoken, as recorded.
	When time.Time
	// Text is the request in the user's own words. Kept verbatim: a
	// paraphrase is the seat's interpretation, and the point of this file is
	// that the request survives without one.
	Text string
}

// line renders a request as one journal line. Tab-separated so a request
// containing punctuation, quotes or commas needs no escaping and survives
// round-tripping unchanged.
func (r Request) line() string {
	return r.When.UTC().Format(time.RFC3339) + "\t" + strings.Join(strings.Fields(r.Text), " ")
}

// Append records a request, creating the file and its directory if needed.
// Appending rather than rewriting means a crash or a full disk loses at most
// the request being written, never the ones already recorded.
func Append(path string, r Request) error {
	if strings.TrimSpace(r.Text) == "" {
		return errors.New("refusing to record an empty request")
	}
	// The same rule the reader applies, applied here too. The launcher lines
	// in the installed journal did not arrive through this function - a
	// wrapper appended them with a shell redirect - but a writer that can add
	// what the reader refuses to read back is a journal that disagrees with
	// itself about what it holds.
	if plumbing(r.Text) {
		return fmt.Errorf("refusing to record %q: that is a command line, not something anybody asked for", oneLine(r.Text))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(r.line() + "\n"); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), f.Close())
	}
	// The close is part of the write: a request that never reached the disk
	// is the failure this package exists to prevent, and a deferred close
	// would hide it.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// Journal is one file's worth of records: the requests a person made, and how
// many lines were not requests at all.
//
// The ignored count is reported rather than absorbed. A reader that silently
// drops lines is the same kind of failure as one that silently invents them,
// and both have now happened to this file.
type Journal struct {
	// Requests are the things asked for aloud, in the order recorded.
	Requests []Request
	// Ignored counts lines refused as plumbing: machinery that wrote its own
	// command line where the agent writes requests.
	Ignored int
}

// Load reads one journal. A missing file is not an error: no requests
// recorded and no file are the same state, and treating the absence as a
// failure would make `voice doctor` report a problem on a healthy install.
func Load(path string) (Journal, error) {
	// Read the whole file rather than streaming it: a request journal is a
	// handful of lines, and this needs no Close whose error would have to be
	// discarded - which this repo forbids and errcheck catches.
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Journal{}, nil
		}
		return Journal{}, fmt.Errorf("read requests: %w", err)
	}
	var j Journal
	for _, raw := range strings.Split(string(body), "\n") {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		// A line with no timestamp at all, or with one that will not parse,
		// is still something a person asked for: it is kept whole rather
		// than dropped or split in half.
		r := Request{Text: text}
		if when, rest, found := strings.Cut(text, "\t"); found {
			if t, perr := time.Parse(time.RFC3339, when); perr == nil {
				r = Request{When: t, Text: strings.TrimSpace(rest)}
			}
		}
		if plumbing(r.Text) {
			j.Ignored++
			continue
		}
		j.Requests = append(j.Requests, r)
	}
	return j, nil
}

// plumbing reports whether a journal line is machinery rather than speech.
//
// The installed journal holds two entries whose text is the agent's own
// launch command:
//
//	HERDR_AGENT=omp exec /usr/local/bin/voice-agent-session
//
// Something in the launch path wrote its command line where the agent writes
// requests, and `voice doctor` read the result back as two spoken feature
// requests. Nobody asked for them, and they sat at the top of the list of
// things a person supposedly wanted.
//
// The rule is deliberately NOT that path, nor that variable name. The next
// wrapper to leak its own invocation will leak a different string, and a list
// of known-bad lines would wave it through while claiming to have checked.
// What this detects is the SHAPE of a command line - environment
// assignments, absolute executable paths, exec and sudo, pipes and
// redirections - and it wants two of those marks before refusing a line,
// because any one of them is something a person can plausibly say out loud.
// "Run /usr/local/bin/backup every night" is a request; "HERDR_AGENT=omp
// exec /usr/local/bin/voice-agent-session" is a command line.
//
// It stays conservative on purpose. Losing a request the owner actually made
// is the failure this package exists to prevent, so a borderline line is
// kept, and every refusal is counted and reported rather than hidden.
func plumbing(text string) bool {
	var assignment, path, program, operator bool
	for _, field := range strings.Fields(text) {
		switch {
		case envAssignment(field):
			assignment = true
		case absolutePath(field):
			path = true
		case shellPrograms[field]:
			program = true
		case shellOperators[field]:
			operator = true
		}
	}
	marks := 0
	for _, seen := range []bool{assignment, path, program, operator} {
		if seen {
			marks++
		}
	}
	return marks >= 2
}

// envAssignment matches the shell's own NAME=value prefix, restricted to the
// upper-case convention units and wrappers follow. Accepting any case would
// refuse "volume=low", which somebody could plausibly dictate.
func envAssignment(field string) bool {
	name, _, ok := strings.Cut(field, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// absolutePath matches an absolute path with a directory component -
// /usr/local/bin/voice-agent-session, not "/" or a bare "/home" - because
// that is how a launcher names the thing it runs.
func absolutePath(field string) bool {
	return strings.HasPrefix(field, "/") && strings.Count(strings.TrimSuffix(field, "/"), "/") >= 2
}

// shellPrograms are the words a launcher uses to start something, which a
// person dictating a feature request does not say on their own.
var shellPrograms = map[string]bool{
	"exec": true, "sudo": true, "env": true, "nohup": true, "setsid": true,
	"systemd-run": true, "systemctl": true, "bash": true, "sh": true,
}

// shellOperators are whole-field shell punctuation. Whole fields only, so
// "turn off the TV; dim the lights" stays a request: the semicolon there is
// attached to a word rather than standing alone as an operator.
var shellOperators = map[string]bool{
	"|": true, "||": true, "&&": true, ";": true, "&": true,
	">": true, ">>": true, "<": true, "2>": true, "2>&1": true,
}

// oneLine folds a request onto one line, as the journal stores it.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
