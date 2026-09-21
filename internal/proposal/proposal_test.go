package proposal

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// base is a fixed wall clock. Every test injects times explicitly, so the
// whole state machine - including a 72 hour expiry - runs in microseconds and
// never depends on when the suite happens to run.
var base = time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)

// store opens the real SQLite store in a temporary directory, so the tests
// exercise the persistence path production uses.
func store(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state", "proposals.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	return st
}

func record(t *testing.T, s *Store, request string, at time.Time) Proposal {
	t.Helper()
	p, _, err := s.Record(Proposal{Request: request, Title: "t", Scope: "s", Risks: "r"}, at)
	if err != nil {
		t.Fatalf("record %q: %v", request, err)
	}
	return p
}

func transition(t *testing.T, s *Store, id string, to Status, detail string, at time.Time) Proposal {
	t.Helper()
	p, err := s.Transition(id, to, detail, at)
	if err != nil {
		t.Fatalf("transition %s to %s: %v", id, to, err)
	}
	return p
}

func TestFreshStoreIsEmpty(t *testing.T) {
	s := store(t)

	counts, err := s.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	for _, status := range []Status{Pending, Notified, Approved, Declined, IssueCreated, Implementing, Completed, Failed, Expired} {
		if counts[status] != 0 {
			t.Errorf("fresh store counts %s = %d, want 0", status, counts[status])
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("fresh store lists %d proposals, want none", len(list))
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on an unknown id = %v, want ErrNotFound", err)
	}
}

func TestRecordFoldsARepeatRequest(t *testing.T) {
	s := store(t)

	first, created, err := s.Record(Proposal{Request: "  Make the thinking sound louder ", Title: "louder thinking"}, base)
	if err != nil || !created {
		t.Fatalf("first record: created=%v err=%v", created, err)
	}
	again, created, err := s.Record(Proposal{Request: "make the\tthinking sound louder!", Title: "raise thinking volume"}, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if created {
		t.Error("a repeat of the same request created a second proposal")
	}
	if again.ID != first.ID {
		t.Errorf("repeat got id %s, want the original %s", again.ID, first.ID)
	}
	if again.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", again.Occurrences)
	}
	if again.Request != "Make the thinking sound louder" {
		t.Errorf("request = %q, want the first wording kept verbatim on one line", again.Request)
	}
	if !again.Created.Equal(first.Created) {
		t.Errorf("created moved from %v to %v", first.Created, again.Created)
	}
	if !again.Updated.After(first.Updated) {
		t.Errorf("updated %v did not advance past %v", again.Updated, first.Updated)
	}
	if again.Title != "raise thinking volume" {
		t.Errorf("title = %q, want the newest interpretation", again.Title)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("store holds %d proposals, want 1", len(list))
	}
}

func TestRecordKeepsDifferentRequestsApart(t *testing.T) {
	s := store(t)

	record(t, s, "remind me at seven", base)
	second, created, err := s.Record(Proposal{Request: "remind me at eight"}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !created {
		t.Fatal("a different request was folded into an existing proposal")
	}
	if Fingerprint("remind me at seven") == Fingerprint("remind me at eight") {
		t.Error("two different requests share a fingerprint")
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("store holds %d proposals, want 2", len(list))
	}
	if list[0].Created.After(list[1].Created) {
		t.Error("list is not oldest first")
	}
	if list[1].ID != second.ID {
		t.Errorf("last listed id = %s, want %s", list[1].ID, second.ID)
	}
}

func TestRecordReopensAfterDecline(t *testing.T) {
	s := store(t)

	p := record(t, s, "let me text people from the kitchen", base)
	record(t, s, "let me text people from the kitchen", base.Add(time.Minute))
	transition(t, s, p.ID, Notified, "asked the owner", base.Add(2*time.Minute))
	declined := transition(t, s, p.ID, Declined, "not while the mic is shared", base.Add(3*time.Minute))
	if declined.Status != Declined {
		t.Fatalf("status = %s, want declined", declined.Status)
	}

	reopened, created, err := s.Record(Proposal{Request: "Let me text people from the kitchen."}, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if created {
		t.Error("reopening created a second proposal instead of reviving the first")
	}
	if reopened.ID != p.ID {
		t.Errorf("reopened id = %s, want %s", reopened.ID, p.ID)
	}
	if reopened.Status != Pending {
		t.Errorf("status = %s, want pending after the owner asked again", reopened.Status)
	}
	if reopened.Occurrences != 3 {
		t.Errorf("occurrences = %d, want 3 (the count is the evidence)", reopened.Occurrences)
	}
	if !reopened.Notified.IsZero() {
		t.Errorf("notified = %v, want zero so the owner is asked again", reopened.Notified)
	}
	// Reopened means askable again: the decline must not leave the proposal
	// stuck outside the state machine.
	if _, err := s.Transition(p.ID, Approved, "yes", base.Add(25*time.Hour)); err != nil {
		t.Errorf("approving a reopened proposal: %v", err)
	}
}

func TestTransitionRefusesIllegalMoves(t *testing.T) {
	s := store(t)

	p := record(t, s, "deploy my code straight to the pi", base)
	transition(t, s, p.ID, Declined, "too dangerous", base.Add(time.Minute))

	// A declined proposal that can be marched to implementing would let the
	// agent skip the owner's answer entirely.
	_, err := s.Transition(p.ID, Implementing, "seat-1", base.Add(2*time.Minute))
	if err == nil {
		t.Fatal("declined proposal moved to implementing")
	}
	if !strings.Contains(err.Error(), string(Declined)) || !strings.Contains(err.Error(), string(Implementing)) {
		t.Errorf("error %q does not name both states", err)
	}
	after, err := s.Get(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != Declined {
		t.Errorf("status = %s after a refused move, want declined", after.Status)
	}

	other := record(t, s, "sing me a sea shanty on demand", base)
	transition(t, s, other.ID, Approved, "go on", base.Add(time.Minute))
	if _, err := s.Transition(other.ID, Completed, "done", base.Add(2*time.Minute)); err == nil {
		t.Error("approved proposal jumped straight to completed")
	}
	if _, err := s.SetAgent(other.ID, "seat-1", base.Add(2*time.Minute)); err == nil {
		t.Error("approved proposal got an implementation seat before an issue existed")
	}
	if _, err := s.Transition("unknown-id", Approved, "", base); !errors.Is(err, ErrNotFound) {
		t.Errorf("transition on an unknown id = %v, want ErrNotFound", err)
	}
	if !LegalTransition(Notified, Notified) {
		t.Error("re-asking an unanswered proposal must be legal")
	}
}

func TestApprovalRunsThroughToCompletion(t *testing.T) {
	s := store(t)

	p := record(t, s, "tell me when the washing machine finishes", base)
	transition(t, s, p.ID, Notified, "asked in the owner seat", base.Add(time.Minute))
	transition(t, s, p.ID, Approved, "owner said yes", base.Add(2*time.Minute))

	issued, err := s.SetIssue(p.ID, 61, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("set issue: %v", err)
	}
	if issued.Status != IssueCreated || issued.Issue != 61 {
		t.Fatalf("after SetIssue: status=%s issue=%d, want issue_created/61", issued.Status, issued.Issue)
	}
	seated, err := s.SetAgent(p.ID, "WashingMachine", base.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("set agent: %v", err)
	}
	if seated.Status != Implementing || seated.Agent != "WashingMachine" {
		t.Fatalf("after SetAgent: status=%s agent=%q", seated.Status, seated.Agent)
	}
	done := transition(t, s, p.ID, Completed, "shipped", base.Add(time.Hour))
	if done.Status != Completed {
		t.Fatalf("status = %s, want completed", done.Status)
	}
	if done.Issue != 61 || done.Agent != "WashingMachine" {
		t.Errorf("completion lost the issue or seat: %+v", done)
	}
	counts, err := s.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts[Completed] != 1 || counts[Pending] != 0 {
		t.Errorf("counts = %v, want one completed and no pending", counts)
	}

	events, err := s.Events(p.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	want := []Status{Pending, Notified, Approved, IssueCreated, Implementing, Completed}
	if len(events) != len(want) {
		t.Fatalf("history has %d events (%v), want %d", len(events), events, len(want))
	}
	for i, status := range want {
		if events[i].Status != status {
			t.Errorf("event %d = %s, want %s", i, events[i].Status, status)
		}
		if i > 0 && events[i].At.Before(events[i-1].At) {
			t.Errorf("event %d happened before event %d", i, i-1)
		}
	}
	if !events[0].At.Equal(base) {
		t.Errorf("first event at %v, want %v", events[0].At, base)
	}
	if _, err := s.Events("unknown-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("events for an unknown id = %v, want ErrNotFound", err)
	}
}

func TestSetIssueRefusesASecondTicket(t *testing.T) {
	s := store(t)

	p := record(t, s, "put the shopping list on the fridge screen", base)
	transition(t, s, p.ID, Approved, "yes", base.Add(time.Minute))
	if _, err := s.SetIssue(p.ID, 70, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("set issue: %v", err)
	}

	// A broker that retried after a timeout must not leave two tickets behind.
	_, err := s.SetIssue(p.ID, 71, base.Add(3*time.Minute))
	if err == nil {
		t.Fatal("a second issue number was accepted")
	}
	if !strings.Contains(err.Error(), "70") || !strings.Contains(err.Error(), "71") {
		t.Errorf("error %q does not name both issue numbers", err)
	}
	repeat, err := s.SetIssue(p.ID, 70, base.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("re-setting the same issue: %v", err)
	}
	if repeat.Issue != 70 || repeat.Status != IssueCreated {
		t.Errorf("idempotent SetIssue changed the proposal: %+v", repeat)
	}
	events, err := s.Events(p.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 3 { // recorded, approved, issue_created
		t.Errorf("history has %d events (%v), want 3", len(events), events)
	}

	unapproved := record(t, s, "order milk when the carton is empty", base)
	if _, err := s.SetIssue(unapproved.ID, 72, base.Add(time.Minute)); err == nil {
		t.Error("an unapproved proposal got a GitHub issue")
	}
}

func TestDueForNoticeRateLimitsAndRetries(t *testing.T) {
	s := store(t)
	notice := Notice{MaxPerHour: 2, RetryAfter: 6 * time.Hour}

	first := record(t, s, "read my email out loud", base)
	second := record(t, s, "turn the hallway light on", base.Add(time.Minute))
	third := record(t, s, "play the radio at breakfast", base.Add(2*time.Minute))

	due, err := s.DueForNotice(base.Add(3*time.Minute), notice)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 2 || due[0].ID != first.ID || due[1].ID != second.ID {
		t.Fatalf("due = %v, want the two oldest pending proposals", ids(due))
	}

	transition(t, s, first.ID, Notified, "asked", base.Add(3*time.Minute))
	transition(t, s, second.ID, Notified, "asked", base.Add(4*time.Minute))

	// The hourly budget is spent, so the third request waits rather than
	// adding a third question to the owner's seat.
	due, err = s.DueForNotice(base.Add(5*time.Minute), notice)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due = %v while the hourly notice budget was spent", ids(due))
	}

	// An hour later the budget has rolled off; the two already asked are not
	// due again until RetryAfter has passed.
	due, err = s.DueForNotice(base.Add(70*time.Minute), notice)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 || due[0].ID != third.ID {
		t.Fatalf("due = %v, want only the never-asked proposal", ids(due))
	}
	transition(t, s, third.ID, Notified, "asked", base.Add(70*time.Minute))

	// Past RetryAfter the unanswered questions come back, oldest first.
	due, err = s.DueForNotice(base.Add(7*time.Hour), notice)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 2 || due[0].ID != first.ID || due[1].ID != second.ID {
		t.Fatalf("due = %v, want the two unanswered proposals retried", ids(due))
	}
	if _, err := s.Transition(first.ID, Notified, "asked again", base.Add(7*time.Hour)); err != nil {
		t.Fatalf("re-notifying: %v", err)
	}

	// Answered proposals drop out entirely.
	transition(t, s, second.ID, Declined, "no", base.Add(8*time.Hour))
	due, err = s.DueForNotice(base.Add(14*time.Hour), notice)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	for _, p := range due {
		if p.ID == second.ID {
			t.Errorf("a declined proposal is still due for notice: %v", ids(due))
		}
	}

	none, err := s.DueForNotice(base.Add(14*time.Hour), Notice{MaxPerHour: 0, RetryAfter: time.Hour})
	if err != nil {
		t.Fatalf("due with no budget: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("due = %v with MaxPerHour 0, want none", ids(none))
	}
}

func TestExpireRetiresUnansweredProposals(t *testing.T) {
	s := store(t)

	stale := record(t, s, "learn to order a taxi", base)
	asked := record(t, s, "wake me with the news", base.Add(time.Minute))
	transition(t, s, asked.ID, Notified, "asked", base.Add(2*time.Hour))
	answered := record(t, s, "dim the lights after ten", base.Add(2*time.Minute))
	transition(t, s, answered.ID, Approved, "yes", base.Add(3*time.Minute))

	gone, err := s.Expire(base.Add(70*time.Hour), 72*time.Hour)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("expired %v before the deadline", ids(gone))
	}

	// Asking again keeps a proposal alive; being notified repeatedly does not.
	record(t, s, "Learn to order a taxi!", base.Add(60*time.Hour))

	gone, err = s.Expire(base.Add(73*time.Hour), 72*time.Hour)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(gone) != 1 || gone[0].ID != asked.ID {
		t.Fatalf("expired %v, want only the unanswered proposal nobody re-asked", ids(gone))
	}
	if gone[0].Status != Expired {
		t.Errorf("returned status = %s, want expired", gone[0].Status)
	}

	alive, err := s.Get(stale.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if alive.Status != Pending {
		t.Errorf("a proposal asked for again is %s, want pending", alive.Status)
	}
	approved, err := s.Get(answered.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if approved.Status != Approved {
		t.Errorf("an approved proposal expired: %s", approved.Status)
	}
	events, err := s.Events(asked.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	last := events[len(events)-1]
	if last.Status != Expired || !last.At.Equal(base.Add(73*time.Hour)) {
		t.Errorf("last event = %+v, want the expiry at the sweep time", last)
	}
}

func TestIDsAreShellSafeAndSortByCreation(t *testing.T) {
	s := store(t)

	// The owner's broker pastes these straight into
	// `sudo -n -u voice-agent voice proposals approve ID`.
	seen := map[string]bool{}
	var previous string
	for i, request := range []string{"one thing", "another thing", "a third thing"} {
		p := record(t, s, request, base.Add(time.Duration(i)*time.Second))
		if strings.ContainsAny(p.ID, " \t\n'\"`$;&|<>()*?[]{}\\!#~") {
			t.Errorf("id %q is not safe as a bare shell argument", p.ID)
		}
		if seen[p.ID] {
			t.Errorf("id %q was reused", p.ID)
		}
		seen[p.ID] = true
		if previous != "" && p.ID <= previous {
			t.Errorf("id %q does not sort after the earlier %q", p.ID, previous)
		}
		previous = p.ID
	}
}

func TestRecordRejectsAnEmptyRequest(t *testing.T) {
	s := store(t)

	if _, _, err := s.Record(Proposal{Request: "   \t "}, base); err == nil {
		t.Fatal("an empty request was recorded")
	}
}

func TestOpenRefusesSomebodyElsesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE timers (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err := Open(path)
	if err == nil {
		t.Fatalf("opened a database belonging to something else")
	}
	if s != nil {
		t.Error("a refused Open returned a store")
	}
	if !strings.Contains(err.Error(), "proposals.file") {
		t.Errorf("error %q does not say which setting to fix", err)
	}
}

func TestStoreSurvivesReopening(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proposals.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := record(t, first, "put the calendar on the speaker", base)
	transition(t, first, p.ID, Notified, "asked", base.Add(time.Minute))
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	got, err := second.Get(p.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Status != Notified || !got.Notified.Equal(base.Add(time.Minute)) {
		t.Errorf("after reopen: status=%s notified=%v", got.Status, got.Notified)
	}
	if got.Request != "put the calendar on the speaker" {
		t.Errorf("request = %q after reopen", got.Request)
	}
	events, err := second.Events(p.ID)
	if err != nil {
		t.Fatalf("events after reopen: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("history has %d events after reopen, want 2", len(events))
	}
}

func ids(ps []Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}
