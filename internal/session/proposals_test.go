package session

import (
	"bytes"
	"context"
	stdlog "log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/proposal"
)

// proposingPlanner stands in for the agent seat answering a request it cannot
// carry out: an honest reply plus the structured proposal.
type proposingPlanner struct {
	reply    string
	proposal brain.Proposal
}

func (p *proposingPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	prop := p.proposal
	return brain.Plan{Speak: p.reply, Proposal: &prop}, nil
}
func (*proposingPlanner) Name() string              { return "proposing planner" }
func (*proposingPlanner) Available() (bool, string) { return true, "available" }

// proposalStore opens a real store in a temp directory and closes it through
// the test's cleanup, where a close failure is reported rather than dropped:
// this store is what the whole feature trusts to remember a request.
func proposalStore(t *testing.T) *proposal.Store {
	t.Helper()
	store, err := proposal.Open(filepath.Join(t.TempDir(), "proposals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close proposal store: %v", err)
		}
	})
	return store
}

func proposalAssistant(t *testing.T, planner brain.Planner, store *proposal.Store, texts ...string) (*Assistant, *recordingSynthesizer, *bytes.Buffer) {
	t.Helper()
	tts := &recordingSynthesizer{}
	journal := &bytes.Buffer{}
	return &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: len(texts)},
		STT:         &queuedTranscriber{texts: texts},
		TTS:         tts,
		Brain:       planner,
		Devices:     device.NewRegistry(),
		Proposals:   store,
		WakePhrases: []string{"hey voice"},
		Logger:      stdlog.New(journal, "", 0),
	}, tts, journal
}

// The whole feature exists because a request the account could not do was
// answered and then forgotten, so the record must survive and the person who
// spoke must hear that it went to the owner.
func TestUnsupportedRequestIsRecordedAndSpokenAsSentToTheOwner(t *testing.T) {
	store := proposalStore(t)
	planner := &proposingPlanner{
		reply: "I can't change that from here",
		proposal: brain.Proposal{
			Request: "make the thinking sound volume configurable",
			Title:   "Configurable thinking sound volume",
			Scope:   "add a volume setting for the thinking sound",
			Risks:   "none beyond a new config field",
		},
	}
	a, tts, journal := proposalAssistant(t, planner, store, "hey voice make the thinking sound volume configurable")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("recorded %d proposals, want 1", len(stored))
	}
	if stored[0].Request != "make the thinking sound volume configurable" {
		t.Fatalf("recorded request %q, want the spoken words verbatim", stored[0].Request)
	}
	said := tts.spoken()
	if !saidContaining(said, "can't change that") {
		t.Fatalf("the answer itself was dropped: %q", said)
	}
	if !saidContaining(said, "sent that to the owner") {
		t.Fatalf("reply never tells the owner it was sent: %q", said)
	}
	if !strings.Contains(journal.String(), "stage=proposal") {
		t.Fatalf("no stage=proposal line in the journal: %q", journal.String())
	}
}

// Asking twice must not look like two separate requests, to the owner's queue
// or to the person waiting: the second answer has to say it is still pending,
// otherwise a listener hears a fresh filing every time and never learns that
// nothing is moving.
func TestRepeatedRequestRecordsOnceAndSaysItIsStillWaiting(t *testing.T) {
	store := proposalStore(t)
	planner := &proposingPlanner{
		reply:    "I can't install software from this account",
		proposal: brain.Proposal{Request: "install the new speech model", Title: "Install speech model"},
	}
	a, tts, _ := proposalAssistant(t, planner, store,
		"hey voice install the new speech model",
		"hey voice install the new speech model",
	)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("recorded %d proposals for the same request, want 1", len(stored))
	}
	said := tts.spoken()
	if len(said) != 2 {
		t.Fatalf("spoke %d replies, want 2: %q", len(said), said)
	}
	if !strings.Contains(strings.ToLower(said[1]), "still waiting") {
		t.Fatalf("repeat reply does not say the request is still waiting: %q", said[1])
	}
	if strings.Contains(strings.ToLower(said[1]), "i've sent") {
		t.Fatalf("repeat reply claims a second request was filed: %q", said[1])
	}
}

// Proposals are optional. Without a store the assistant must still answer,
// because a missing database is not a reason to stop talking.
func TestNoProposalStoreStillAnswers(t *testing.T) {
	planner := &proposingPlanner{
		reply:    "I can't do that from here",
		proposal: brain.Proposal{Request: "rewrite the wake word detector", Title: "New wake word detector"},
	}
	a, tts, _ := proposalAssistant(t, planner, nil, "hey voice rewrite the wake word detector")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !saidContaining(tts.spoken(), "can't do that") {
		t.Fatalf("answer lost when no store is configured: %q", tts.spoken())
	}
}

// A broken store costs the record, never the reply: silence would be the
// worst of both, the request neither kept nor answered.
func TestProposalStoreFailureStillSpeaksTheAnswer(t *testing.T) {
	store := proposalStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	planner := &proposingPlanner{
		reply:    "I can't reach the code from here",
		proposal: brain.Proposal{Request: "add a bedtime routine", Title: "Bedtime routine"},
	}
	a, tts, _ := proposalAssistant(t, planner, store, "hey voice add a bedtime routine")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !saidContaining(tts.spoken(), "can't reach the code") {
		t.Fatalf("answer lost when the store failed: %q", tts.spoken())
	}
}
