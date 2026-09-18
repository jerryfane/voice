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

// Load reads every recorded request. A missing file is not an error: no
// requests recorded and no file are the same state, and treating the absence
// as a failure would make `voice doctor` report a problem on a healthy
// install.
func Load(path string) ([]Request, error) {
	// Read the whole file rather than streaming it: a request journal is a
	// handful of lines, and this needs no Close whose error would have to be
	// discarded - which this repo forbids and errcheck catches.
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []Request
	for _, raw := range strings.Split(string(body), "\n") {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		when, rest, found := strings.Cut(text, "\t")
		if !found {
			// A line without a timestamp is still a request someone made.
			// Dropping it would lose the thing this file exists to keep.
			out = append(out, Request{Text: text})
			continue
		}
		r := Request{Text: strings.TrimSpace(rest)}
		if t, perr := time.Parse(time.RFC3339, when); perr == nil {
			r.When = t
		} else {
			// Unparseable timestamp: keep the request whole rather than
			// silently discarding either half.
			r.Text = text
		}
		out = append(out, r)
	}
	return out, nil
}
