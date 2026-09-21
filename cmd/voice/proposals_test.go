package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/proposal"
	"github.com/jerryfane/voice/internal/session"
)

// These tests drive the commands the way the owner's broker does: arguments in,
// printed lines and an exit status out. That is the whole contract - the broker
// runs `sudo -n -u voice-agent voice proposals ...` and has nothing else to go
// on - so anything it reads has to be asserted here rather than assumed.

func proposalsFixture(t *testing.T) (config.Config, *session.Assistant) {
	t.Helper()
	c := config.Default()
	c.Proposals.File = filepath.Join(t.TempDir(), "proposals.db")
	c.Proposals.Approver = "owner-seat"
	store, err := proposal.Open(c.Proposals.File)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// The session opens the store at startup and the subcommand reuses that
	// handle, so this is the arrangement production runs in.
	return c, &session.Assistant{Proposals: store}
}

// proposals runs one subcommand and returns what it printed.
func runCLI(t *testing.T, c config.Config, a *session.Assistant, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	saved := stdout
	stdout = &printer{w: &buf}
	err := proposalsCommand(a, c, "/etc/voice/config.json", args)
	stdout = saved
	return buf.String(), err
}

// ok runs a subcommand that must succeed and returns its output.
func mustRun(t *testing.T, c config.Config, a *session.Assistant, args ...string) string {
	t.Helper()
	out, err := runCLI(t, c, a, args...)
	if err != nil {
		t.Fatalf("voice proposals %s = %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// collapse folds the column padding away, so an assertion reads like the line
// it is checking instead of counting spaces.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func idFrom(t *testing.T, out string) string {
	t.Helper()
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("no proposal id in %q", out)
	}
	return fields[0]
}

// The path the feature exists for, end to end: a request Voice could not serve
// becomes a record, the owner is asked, the owner approves, an issue and a seat
// are attached, and the work finishes. Every step is a separate process in
// production - the session records, the owner's broker does the rest - so each
// one has to work from nothing but an ID on a command line.
func TestAProposalRunsFromRequestToCompletionOnTheCommandLine(t *testing.T) {
	c, a := proposalsFixture(t)
	spoken := "make the thinking sound's volume configurable"

	out := mustRun(t, c, a, "record", spoken,
		"--title", "Configurable thinking volume",
		"--scope", "config field plus a doctor line",
		"--risks", "none material")
	if !strings.Contains(out, "recorded") {
		t.Fatalf("record printed %q", out)
	}
	id := idFrom(t, out)

	// Asked again, in a worse mood. One proposal, counted twice: a second
	// approval question for the same missing feature is how the owner learns
	// to ignore them.
	out = mustRun(t, c, a, "record", "Make the thinking sound's volume configurable!")
	if !strings.Contains(out, id) || !strings.Contains(out, "already recorded") {
		t.Errorf("a repeat of the same request printed %q, want it folded into %s", out, id)
	}
	if !strings.Contains(out, "2 time(s)") {
		t.Errorf("record printed %q, want the occurrence count", out)
	}
	if out := mustRun(t, c, a, "list"); strings.Count(out, id) != 1 {
		t.Errorf("list printed %q; the repeat must not appear as a second proposal", out)
	}

	// Nobody has been asked yet, so the broker has something to carry.
	if out := mustRun(t, c, a, "due"); !strings.Contains(out, id) {
		t.Errorf("due printed %q, want %s waiting for the owner", out, id)
	}
	if out := mustRun(t, c, a, "show", id); !strings.Contains(out, spoken) {
		t.Errorf("show printed %q, want the request verbatim", out)
	}

	// Asked, then answered. Between the two, due must go quiet: the retry
	// interval is what stops one missing capability becoming a stream of
	// identical questions.
	if out := mustRun(t, c, a, "notified", id); !strings.Contains(out, string(proposal.Notified)) {
		t.Errorf("notified printed %q", out)
	}
	if out := mustRun(t, c, a, "due"); strings.Contains(out, id) {
		t.Errorf("due printed %q straight after the owner was asked; %s must wait out retry_after", out, id)
	}
	if out := mustRun(t, c, a, "approve", id); !strings.Contains(out, string(proposal.Approved)) {
		t.Errorf("approve printed %q", out)
	}
	out = mustRun(t, c, a, "issue", id, "91")
	if !strings.Contains(out, "#91") || !strings.Contains(out, string(proposal.IssueCreated)) {
		t.Errorf("issue printed %q, want the number and the new status", out)
	}
	out = mustRun(t, c, a, "implementing", id, "voice-53-seat")
	if !strings.Contains(out, "voice-53-seat") || !strings.Contains(out, string(proposal.Implementing)) {
		t.Errorf("implementing printed %q, want the seat and the new status", out)
	}
	if out := mustRun(t, c, a, "complete", id); !strings.Contains(out, string(proposal.Completed)) {
		t.Errorf("complete printed %q", out)
	}

	out = mustRun(t, c, a, "show", id)
	for _, want := range []string{string(proposal.Completed), "#91", "voice-53-seat", spoken} {
		if !strings.Contains(out, want) {
			t.Errorf("show printed %q, missing %q", out, want)
		}
	}
}

// A decline is an answer, and the reason is the only record of why this was
// never built. It also has to stop the asking: a declined proposal that kept
// coming back as due would put the owner's own no in front of them again.
func TestADeclineKeepsTheReasonAndStopsTheAsking(t *testing.T) {
	c, a := proposalsFixture(t)
	id := idFrom(t, mustRun(t, c, a, "record", "let me dictate emails"))
	reason := "not now, the microphone work comes first"

	if out := mustRun(t, c, a, "decline", id, reason); !strings.Contains(out, string(proposal.Declined)) {
		t.Errorf("decline printed %q", out)
	}
	if out := mustRun(t, c, a, "show", id); !strings.Contains(out, reason) {
		t.Errorf("show printed %q, want the owner's reason kept", out)
	}
	if out := mustRun(t, c, a, "due"); strings.Contains(out, id) {
		t.Errorf("due printed %q; a declined proposal must not be asked about again", out)
	}
}

// A seat that would not start is recorded against the proposal rather than
// lost in the broker's log, because the proposal is the only place anybody
// looks to find out what became of the request.
func TestALaunchFailureIsRecordedAgainstTheProposal(t *testing.T) {
	c, a := proposalsFixture(t)
	id := idFrom(t, mustRun(t, c, a, "record", "read my calendar out in the morning"))
	mustRun(t, c, a, "approve", id)

	detail := "herdr refused to start the seat"
	if out := mustRun(t, c, a, "fail", id, detail); !strings.Contains(out, string(proposal.Failed)) {
		t.Errorf("fail printed %q", out)
	}
	if out := mustRun(t, c, a, "show", id); !strings.Contains(out, detail) {
		t.Errorf("show printed %q, want the failure detail", out)
	}
}

// Everything the owner's broker reads comes through --json, so the keys and
// their shapes are an interface. A renamed key breaks an approval the owner
// already gave.
func TestJSONCarriesTheDocumentedKeys(t *testing.T) {
	c, a := proposalsFixture(t)
	spoken := "turn the hallway light on at sunset"
	id := idFrom(t, mustRun(t, c, a, "record", spoken, "--title", "Sunset schedule", "--scope", "timer plus device call", "--risks", "wakes the house"))
	mustRun(t, c, a, "notified", id)
	mustRun(t, c, a, "approve", id)
	mustRun(t, c, a, "issue", id, "42")

	var got []map[string]any
	if err := json.Unmarshal([]byte(mustRun(t, c, a, "list", "--json")), &got); err != nil {
		t.Fatalf("list --json did not decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list --json returned %d proposals, want 1", len(got))
	}
	row := got[0]
	for _, key := range []string{
		"id", "request", "title", "scope", "risks", "status", "occurrences",
		"issue", "agent", "detail", "created", "updated", "notified",
	} {
		if _, present := row[key]; !present {
			t.Errorf("list --json has no %q key: %v", key, row)
		}
	}
	if row["id"] != id || row["request"] != spoken || row["title"] != "Sunset schedule" {
		t.Errorf("list --json = %v, want the recorded id, verbatim request and title", row)
	}
	if row["status"] != string(proposal.IssueCreated) {
		t.Errorf("status = %v, want %q", row["status"], proposal.IssueCreated)
	}
	if n, isNumber := row["issue"].(float64); !isNumber || int(n) != 42 {
		t.Errorf("issue = %v, want the number 42 so a broker can compare it", row["issue"])
	}
	when, isString := row["notified"].(string)
	if !isString {
		t.Fatalf("notified = %v, want a string", row["notified"])
	}
	if _, err := time.Parse(time.RFC3339, when); err != nil {
		t.Errorf("notified = %q, which does not parse as RFC3339: %v", when, err)
	}
}

// An empty board must be an empty array. The broker pipes this into jq, where
// null has no length and a missing feature would look like a broker bug.
func TestJSONOfAnEmptyBoardIsAnEmptyArray(t *testing.T) {
	c, a := proposalsFixture(t)
	for _, command := range [][]string{{"list", "--json"}, {"due", "--json"}} {
		var got []map[string]any
		out := mustRun(t, c, a, command...)
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v printed %q, which does not decode: %v", command, out, err)
		}
		if got == nil {
			t.Errorf("%v printed %q, want an empty array", command, strings.TrimSpace(out))
		}
		if len(got) != 0 {
			t.Errorf("%v returned %d proposals from an empty store", command, len(got))
		}
	}
}

// A proposal the owner never answered has to age out, or an unread question
// from days ago competes with today's. Every proposal here is minutes old, so
// the sweep is driven by the configured window rather than by sleeping.
func TestExpireRetiresTheProposalsNobodyAnswered(t *testing.T) {
	c, a := proposalsFixture(t)
	id := idFrom(t, mustRun(t, c, a, "record", "order milk when the fridge is empty"))

	if out := mustRun(t, c, a, "expire"); strings.Contains(out, id) {
		t.Errorf("expire printed %q; a proposal recorded seconds ago is not stale", out)
	}
	c.Proposals.Expire = config.Duration(time.Nanosecond)
	if out := mustRun(t, c, a, "expire"); !strings.Contains(out, id) {
		t.Errorf("expire printed %q, want %s retired", out, id)
	}
	if out := mustRun(t, c, a, "show", id); !strings.Contains(out, string(proposal.Expired)) {
		t.Errorf("show printed %q, want %q", out, proposal.Expired)
	}
}

// An unknown ID must exit nonzero and name the ID. The broker passes IDs it
// read from an earlier due list, so a miss means the record moved, and it has
// nothing but that ID to report.
func TestAnUnknownIDFailsAndNamesIt(t *testing.T) {
	c, a := proposalsFixture(t)
	for _, args := range [][]string{
		{"show", "20260101T000000-deadbeef00"},
		{"notified", "20260101T000000-deadbeef01"},
		{"approve", "20260101T000000-deadbeef02"},
		{"decline", "20260101T000000-deadbeef03", "no"},
		{"issue", "20260101T000000-deadbeef04", "7"},
		{"implementing", "20260101T000000-deadbeef05", "seat"},
		{"complete", "20260101T000000-deadbeef06"},
		{"fail", "20260101T000000-deadbeef07", "broke"},
	} {
		out, err := runCLI(t, c, a, args...)
		if err == nil {
			t.Errorf("voice proposals %s succeeded on an unknown id", strings.Join(args, " "))
			continue
		}
		if !strings.Contains(err.Error(), args[1]) {
			t.Errorf("voice proposals %s failed with %q, which does not name the id", strings.Join(args, " "), err)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("voice proposals %s printed %q for an unknown id", strings.Join(args, " "), out)
		}
	}
}

// Disabled means disabled, and the message has to say which setting turned it
// off: the operator reading it is looking at a broker that just failed, not at
// this source.
func TestEveryCommandFailsWhenProposalsAreDisabled(t *testing.T) {
	c := config.Default()
	c.Proposals.Enabled = false
	a := &session.Assistant{}

	for _, args := range [][]string{
		{"list"}, {"list", "--json"}, {"due"}, {"record", "something"},
		{"approve", "any-id"}, {"expire"}, {"show", "any-id"},
	} {
		out, err := runCLI(t, c, a, args...)
		if err == nil {
			t.Errorf("voice proposals %s succeeded with the feature disabled", strings.Join(args, " "))
			continue
		}
		for _, want := range []string{"proposals.enabled", "/etc/voice/config.json"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("voice proposals %s failed with %q, missing %q", strings.Join(args, " "), err, want)
			}
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("voice proposals %s printed %q while disabled", strings.Join(args, " "), out)
		}
	}
}

// doctorProposals runs the doctor report and returns its lines plus the checks
// that failed, which is what decides doctor's own exit status.
func doctorProposals(t *testing.T, c config.Config, a *session.Assistant) (string, []string) {
	t.Helper()
	var buf bytes.Buffer
	var failed []string
	saved := stdout
	stdout = &printer{w: &buf}
	proposalsDoctor(a, c, func(label string, passed bool, detail string) {
		status := "OK"
		if !passed {
			status = "FAIL"
			failed = append(failed, label+": "+detail)
		}
		stdout.printf("%-12s %-4s %s\n", label, status, detail)
	})
	stdout = saved
	return buf.String(), failed
}

// doctor is where the board becomes visible to a person, so the counts have to
// be there by status. The issue asked for exactly this, because the old report
// listed the agent's launch command as a feature request and nobody could tell
// from the output that anything was wrong.
func TestDoctorReportsProposalCountsByStatus(t *testing.T) {
	c, a := proposalsFixture(t)
	mustRun(t, c, a, "record", "dictate emails")
	declined := idFrom(t, mustRun(t, c, a, "record", "order milk automatically"))
	mustRun(t, c, a, "decline", declined, "not now")

	out, failed := doctorProposals(t, c, a)
	line := collapse(out)
	for _, want := range []string{"2 recorded", "pending 1", "declined 1"} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor reported %q, missing %q", out, want)
		}
	}
	if len(failed) != 0 {
		t.Errorf("doctor failed %v on a healthy board", failed)
	}
}

// An empty board is the healthy case and must read like one. A queue with
// nothing in it reported as a warning is how people learn to skim past this
// output, which is the habit that lost the original request.
func TestDoctorIsQuietWhenNothingHasBeenProposed(t *testing.T) {
	c, a := proposalsFixture(t)

	out, failed := doctorProposals(t, c, a)
	if len(failed) != 0 {
		t.Errorf("doctor failed %v with an empty, healthy store", failed)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("doctor reported %q for an empty store", out)
	}
	if !strings.Contains(collapse(out), "proposals OK none recorded") {
		t.Errorf("doctor reported %q, want a plain line saying nothing is recorded", out)
	}
}

// A store that could not be opened is a device that cannot record anything the
// owner asks for, and silently the same as before this feature existed. It has
// to fail, and it has to name the file.
func TestDoctorFailsWhenTheStoreIsEnabledButUnavailable(t *testing.T) {
	c := config.Default()
	c.Proposals.Approver = "owner-seat"
	c.Proposals.File = filepath.Join(t.TempDir(), "proposals.db")

	out, failed := doctorProposals(t, c, &session.Assistant{})
	if len(failed) != 1 {
		t.Fatalf("doctor failed %v, want exactly one failing check; output:\n%s", failed, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("doctor reported %q, want a failing line", out)
	}
}

// An empty approver passes config validation on purpose, because the installer
// fills it in later. That makes doctor the only place it can surface: a
// proposal nobody can be asked to approve waits forever, which is the
// vanishing request this feature exists to end.
func TestDoctorFlagsAnUnconfiguredApprover(t *testing.T) {
	c, a := proposalsFixture(t)
	c.Proposals.Approver = ""
	mustRun(t, c, a, "record", "let me dictate emails")

	out, failed := doctorProposals(t, c, a)
	if len(failed) != 1 {
		t.Fatalf("doctor failed %v, want one failing check for the empty approver; output:\n%s", failed, out)
	}
	if !strings.Contains(failed[0], "proposals.approver") {
		t.Errorf("the failing check said %q, which does not name the setting to fill in", failed[0])
	}
}

// Disabled is a choice, not a fault: doctor must say nothing at all, or every
// device that turned the feature off reports a problem forever.
func TestDoctorSaysNothingWhenProposalsAreDisabled(t *testing.T) {
	c := config.Default()
	c.Proposals.Enabled = false

	out, failed := doctorProposals(t, c, &session.Assistant{})
	if out != "" || len(failed) != 0 {
		t.Errorf("doctor reported %q / %v with proposals disabled", out, failed)
	}
}
