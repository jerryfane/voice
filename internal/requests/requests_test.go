package requests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A spoken request must survive verbatim. The one that prompted this package
// was lost entirely, and a paraphrase would have been almost as bad: the
// owner's words are the record, and punctuation is part of them.
func TestARecordedRequestSurvivesVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "REQUESTS.tsv")
	spoken := "Can you make the thinking sound's volume configurable? It's too quiet."
	when := time.Date(2026, 9, 18, 14, 5, 0, 0, time.UTC)

	if err := Append(path, Request{When: when, Text: spoken}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d requests, want 1", len(got))
	}
	if got[0].Text != spoken {
		t.Errorf("request came back as %q, want %q", got[0].Text, spoken)
	}
	if !got[0].When.Equal(when) {
		t.Errorf("time came back as %v, want %v", got[0].When, when)
	}
}

// Several requests accumulate in order, and a later one must not overwrite an
// earlier one - the agent appends across separate turns, days apart.
func TestRequestsAccumulateInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	for _, s := range []string{"first thing", "second thing", "third thing"} {
		if err := Append(path, Request{When: time.Now(), Text: s}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("loaded %d requests, want 3", len(got))
	}
	for i, want := range []string{"first thing", "second thing", "third thing"} {
		if got[i].Text != want {
			t.Errorf("request %d = %q, want %q", i, got[i].Text, want)
		}
	}
}

// No file and no requests are the same state. Reporting a missing file as an
// error would make voice doctor announce a problem on a healthy install.
func TestNoFileIsNotAnError(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.tsv"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("loaded %d requests from a missing file", len(got))
	}
}

// A malformed line is still something a person asked for. Dropping it would
// lose exactly what this file exists to keep, so it is kept as text.
func TestAMalformedLineIsKeptRatherThanDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	body := "not-a-timestamp\tplay some music when I get home\n" +
		"a line with no tab at all\n" +
		"\n" +
		time.Now().UTC().Format(time.RFC3339) + "\tproper one\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("loaded %d requests, want 3: blank lines skipped, malformed ones kept", len(got))
	}
	if !strings.Contains(got[0].Text, "play some music") {
		t.Errorf("a request with an unparseable timestamp lost its text: %q", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "no tab") {
		t.Errorf("a request with no timestamp was dropped: %q", got[1].Text)
	}
}

// An empty request is a bug in the caller, not a thing to record: a file of
// blank entries would make the count meaningless.
func TestAnEmptyRequestIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	for _, s := range []string{"", "   ", "\t\n"} {
		if err := Append(path, Request{Text: s}); err == nil {
			t.Errorf("an empty request %q was recorded", s)
		}
	}
	if got, _ := Load(path); len(got) != 0 {
		t.Errorf("loaded %d requests after only empty ones were refused", len(got))
	}
}
