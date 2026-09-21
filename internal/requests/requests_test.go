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
	if len(got.Requests) != 1 {
		t.Fatalf("loaded %d requests, want 1", len(got.Requests))
	}
	if got.Requests[0].Text != spoken {
		t.Errorf("request came back as %q, want %q", got.Requests[0].Text, spoken)
	}
	if !got.Requests[0].When.Equal(when) {
		t.Errorf("time came back as %v, want %v", got.Requests[0].When, when)
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
	if len(got.Requests) != 3 {
		t.Fatalf("loaded %d requests, want 3", len(got.Requests))
	}
	for i, want := range []string{"first thing", "second thing", "third thing"} {
		if got.Requests[i].Text != want {
			t.Errorf("request %d = %q, want %q", i, got.Requests[i].Text, want)
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
	if len(got.Requests) != 0 {
		t.Errorf("loaded %d requests from a missing file", len(got.Requests))
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
	if len(got.Requests) != 3 {
		t.Fatalf("loaded %d requests, want 3: blank lines skipped, malformed ones kept", len(got.Requests))
	}
	if got.Ignored != 0 {
		t.Errorf("%d line(s) ignored; a malformed request is still a request", got.Ignored)
	}
	if !strings.Contains(got.Requests[0].Text, "play some music") {
		t.Errorf("a request with an unparseable timestamp lost its text: %q", got.Requests[0].Text)
	}
	if !strings.Contains(got.Requests[1].Text, "no tab") {
		t.Errorf("a request with no timestamp was dropped: %q", got.Requests[1].Text)
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
	if got, _ := Load(path); len(got.Requests) != 0 {
		t.Errorf("loaded %d requests after only empty ones were refused", len(got.Requests))
	}
}

// The installed journal holds two entries whose text is the agent's own
// launch command - HERDR_AGENT=omp exec /usr/local/bin/voice-agent-session -
// and `voice doctor` reported them as things a person had asked for. They
// were the first two "feature requests" anyone reading that list would see.
//
// A launcher line must not load as a request, and the request beside it must
// survive untouched: a filter that eats real speech would be worse than the
// noise it removes.
func TestALauncherLineIsNotLoadedAsARequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	stamp := time.Now().UTC().Format(time.RFC3339)
	body := stamp + "\tHERDR_AGENT=omp exec /usr/local/bin/voice-agent-session\n" +
		stamp + "\tmake the thinking sound's volume configurable\n" +
		"HERDR_AGENT=omp exec /usr/local/bin/voice-agent-session\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != 1 {
		t.Fatalf("loaded %d requests, want only the spoken one: %+v", len(got.Requests), got.Requests)
	}
	if got.Requests[0].Text != "make the thinking sound's volume configurable" {
		t.Errorf("the spoken request came back as %q", got.Requests[0].Text)
	}
	if got.Ignored != 2 {
		t.Errorf("Ignored = %d, want 2: a dropped line must be counted so doctor can say so, not hidden", got.Ignored)
	}
}

// The rule is the shape of a command line, not a list of paths seen once. A
// request that happens to name a program is still a request, and losing one
// of those is the failure this package exists to prevent.
func TestRequestsThatMentionCommandsAreStillRequests(t *testing.T) {
	spoken := []string{
		"run /usr/local/bin/backup every night at three",
		"can you make sudo work without a password for the light script",
		"turn off the TV; then dim the lights",
		"set volume=low when it's after ten",
	}
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	for _, s := range spoken {
		if err := Append(path, Request{When: time.Now(), Text: s}); err != nil {
			t.Fatalf("Append(%q) = %v; a request naming a command is still a request", s, err)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != len(spoken) || got.Ignored != 0 {
		t.Fatalf("loaded %d requests and ignored %d, want %d and 0: %+v",
			len(got.Requests), got.Ignored, len(spoken), got.Requests)
	}
}

// What the reader refuses, the writer must refuse too, or the file disagrees
// with itself about what it holds.
func TestALauncherLineIsRefusedOnTheWayIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "REQUESTS.tsv")
	if err := Append(path, Request{When: time.Now(), Text: "HERDR_AGENT=omp exec /usr/local/bin/voice-agent-session"}); err == nil {
		t.Fatal("a launch command was recorded as a spoken request")
	}
	if got, _ := Load(path); len(got.Requests) != 0 {
		t.Errorf("loaded %d requests after only a launcher line was offered", len(got.Requests))
	}
}
