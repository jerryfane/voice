package session

import (
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/brain"
)

func TestBusyAmbientFollowUpDoesNotSpeakOrCallFront(t *testing.T) {
	for _, withFront := range []bool{false, true} {
		name := "local"
		if withFront {
			name = "optional front"
		}
		t.Run(name, func(t *testing.T) {
			a, in, gate, synth, _ := conversationAssistant("slow request", "ordinary background discussion", "What is the status?")
			planner := &waitingPlanner{started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}), plan: brain.Plan{Speak: "Finished."}}
			a.Brain = planner
			a.InFlightWindow = time.Minute
			a.InFlightTurns = 2
			front := &testFrontDesk{}
			if withFront {
				a.Front = front
			}
			release := sync.OnceFunc(func() { close(planner.release) })
			defer release()
			finish := runConversation(t, a, in)
			injectWake(in)
			awaitAsync(t, planner.started)
			gate.awaitWindow(t, 0, true)
			injectBusy(in)
			injectBusy(in) // requested status still responds while the planner is blocked
			awaitAsync(t, synth.spoken)
			release()
			finish()
			if synth.calls.Load() != 2 {
				t.Fatalf("got %d spoken turns; want only requested status and final answer", synth.calls.Load())
			}
			if planner.calls.Load() != 1 || front.calls.Load() != 0 {
				t.Fatalf("ambient speech reached a model: planner=%d front=%d", planner.calls.Load(), front.calls.Load())
			}
		})
	}
}
