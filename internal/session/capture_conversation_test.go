package session

import (
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/brain"
)

type interruptedConversationGate struct {
	*observedConversationGate
	faults chan struct{}
}

func (g *interruptedConversationGate) Discontinuities() <-chan struct{} { return g.faults }

func TestCaptureLossPreventsPendingAnswerReopeningConversation(t *testing.T) {
	a, in, gate, synth, _ := conversationAssistant("slow request")
	faults := make(chan struct{}, 1)
	a.VAD = &interruptedConversationGate{gate, faults}
	planner := &waitingPlanner{started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}), plan: brain.Plan{Speak: "Finished."}}
	a.Brain = planner
	a.InFlightWindow, a.InFlightTurns = time.Minute, 2
	release := sync.OnceFunc(func() { close(planner.release) })
	defer release()
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, planner.started)
	gate.awaitWindow(t, 0, true)
	faults <- struct{}{}
	gate.awaitWindow(t, 0, false)
	release()
	if got := awaitAsync(t, synth.spoken); got != "Finished." {
		t.Fatalf("reply=%q", got)
	}
	gate.awaitWindow(t, 1, false)
	injectBusy(in)
	finish()
	if planner.calls.Load() != 1 || a.STT.(*queuedTranscriber).next != 1 {
		t.Fatal("capture loss authorized another wake-free request")
	}
}

func TestCaptureLossClosesIdleConversationBeforeAmbientTranscription(t *testing.T) {
	a, in, gate, synth, planner := conversationAssistant("first", "fresh wake")
	faults := make(chan struct{}, 1)
	a.VAD = &interruptedConversationGate{gate, faults}
	finish := runConversation(t, a, in)
	injectWake(in)
	awaitAsync(t, synth.spoken)
	gate.awaitWindow(t, 1, true)
	faults <- struct{}{}
	gate.awaitWindow(t, 1, false)
	injectBusy(in)
	injectWake(in)
	if got := awaitAsync(t, synth.spoken); got != "Answer to fresh wake" {
		t.Fatalf("fresh reply=%q", got)
	}
	finish()
	if planner.calls.Load() != 2 || a.STT.(*queuedTranscriber).next != 2 {
		t.Fatal("ambient input crossed capture discontinuity")
	}
}
