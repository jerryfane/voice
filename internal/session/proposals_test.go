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
	"github.com/jerryfane/voice/internal/vad"
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

// A restricted request must survive without implying it has started.
func TestUnsupportedRequestIsRecordedPendingApproval(t *testing.T) {
	store := proposalStore(t)
	planner := &proposingPlanner{
		reply: "I assigned this to an implementation agent.",
		proposal: brain.Proposal{
			Request: "make the thinking sound volume configurable",
			Title:   "Configurable thinking sound volume",
			Scope:   "add a volume setting for the thinking sound",
			Risks:   "none beyond a new config field",
		},
	}
	a, tts, _ := proposalAssistant(t, planner, store, "hey voice make the thinking sound volume configurable")
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
	if stored[0].Status != proposal.Pending || !stored[0].Notified.IsZero() || stored[0].Issue != 0 || stored[0].Agent != "" {
		t.Fatalf("request advanced without owner approval: %+v", stored[0])
	}
	said := tts.spoken()
	if len(said) != 1 || strings.Contains(said[0], planner.reply) {
		t.Fatalf("unverified assignment claim escaped into speech: %q", said)
	}
}

// Repeated requests retain one pending approval, not multiple assignments.
func TestRepeatedRequestRecordsOnce(t *testing.T) {
	store := proposalStore(t)
	planner := &proposingPlanner{
		reply:    "I can't install software from this account",
		proposal: brain.Proposal{Request: "install the new speech model", Title: "Install speech model"},
	}
	a, _, _ := proposalAssistant(t, planner, store,
		"hey voice install the new speech model",
		"hey voice install the new speech model",
	)
	utterances := make(chan vad.Utterance)
	a.VAD = injectedSegmenter{utterances}
	synth := &signalingSynthesizer{spoken: make(chan string, 2)}
	a.TTS = synth
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	injectWake(utterances)
	awaitAsync(t, synth.spoken)
	// A repeat after the first response is complete uses the stored
	// proposal; an in-flight repeat is instead answered by the busy path.
	injectWake(utterances)
	awaitAsync(t, synth.spoken)
	close(utterances)
	if err := awaitAsync(t, done); err != nil {
		t.Fatal(err)
	}

	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("recorded %d proposals for the same request, want 1", len(stored))
	}
	if stored[0].Occurrences != 2 || stored[0].Status != proposal.Pending {
		t.Fatalf("repeat changed approval or lost occurrence: %+v", stored[0])
	}
}

// Disabled approvals must not repeat an invented assignment.
func TestNoProposalStoreDoesNotClaimHandoff(t *testing.T) {
	planner := &proposingPlanner{
		reply:    "I assigned this to an implementation agent.",
		proposal: brain.Proposal{Request: "rewrite the wake word detector", Title: "New wake word detector"},
	}
	a, tts, _ := proposalAssistant(t, planner, nil, "hey voice rewrite the wake word detector")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if said := tts.spoken(); len(said) != 1 || strings.Contains(said[0], planner.reply) {
		t.Fatalf("unverified assignment claim escaped into speech: %q", said)
	}
}

// A failed write must not sound like successful dispatch.
func TestProposalStoreFailureDoesNotClaimHandoff(t *testing.T) {
	store := proposalStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	planner := &proposingPlanner{
		reply:    "I assigned this to an implementation agent.",
		proposal: brain.Proposal{Request: "add a bedtime routine", Title: "Bedtime routine"},
	}
	a, tts, _ := proposalAssistant(t, planner, store, "hey voice add a bedtime routine")
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if said := tts.spoken(); len(said) != 1 || strings.Contains(said[0], planner.reply) {
		t.Fatalf("unverified assignment claim escaped into speech: %q", said)
	}
}

func TestPreparedPlanPreservesRequestOnActionFailure(t *testing.T) {
	store := proposalStore(t)
	a := &Assistant{Devices: device.NewRegistry(), Proposals: store}
	_, err := a.ApplyPlan(context.Background(), brain.Plan{
		Proposal: &brain.Proposal{Request: "add a bedtime routine"},
		Actions:  []brain.Action{{Device: "missing", Op: device.OpOn}},
	})
	if err == nil {
		t.Fatal("missing device must fail")
	}
	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Request != "add a bedtime routine" || stored[0].Status != proposal.Pending {
		t.Fatalf("device failure lost or approved the request: %+v", stored)
	}
}

func TestCancelledPreparedPlanDoesNotRecordRequest(t *testing.T) {
	store := proposalStore(t)
	a := &Assistant{Devices: device.NewRegistry(), Proposals: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.ApplyPlan(ctx, brain.Plan{Proposal: &brain.Proposal{Request: "add a bedtime routine"}})
	if err != context.Canceled {
		t.Fatalf("cancelled plan returned %v", err)
	}
	stored, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("cancelled request was persisted: %+v", stored)
	}
}
