package session

import (
	"context"
	"testing"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/vad"
)

func TestRandomTranscriptsNeverReachPlanner(t *testing.T) {
	planner := &countingPlanner{}
	a := &Assistant{
		Recorder:    testRecorder{},
		Player:      testPlayer{},
		VAD:         testSegmenter{count: 3},
		STT:         &queuedTranscriber{texts: []string{"the television is loud", "someone said hey voice turn on the light", "hey voise turn on the light"}},
		TTS:         testSynthesizer{},
		Brain:       planner,
		Devices:     device.NewRegistry(),
		WakePhrases: []string{"hey voice"},
		WakeFuzz:    0,
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 0 {
		t.Fatalf("planner called %d times for non-wake transcripts", planner.calls)
	}
}

func TestLocalControlStopsWithoutPlanner(t *testing.T) {
	reply, stop, handled := localControl("turn yourself off")
	if !handled || !stop || reply != "Turning off." {
		t.Fatalf("got reply=%q stop=%v handled=%v", reply, stop, handled)
	}
}

func TestLocalControlDoesNotCaptureDeviceCommand(t *testing.T) {
	_, _, handled := localControl("turn off the tv")
	if handled {
		t.Fatal("device command must reach the planner")
	}
}

func TestLocalControlReportsAlreadyOnWithoutStopping(t *testing.T) {
	reply, stop, handled := localControl("turn on")
	if !handled || stop || reply != "I'm already on." {
		t.Fatalf("got reply=%q stop=%v handled=%v", reply, stop, handled)
	}
}

type testRecorder struct{}

func (testRecorder) Stream(context.Context) (<-chan []int16, <-chan error) {
	pcm := make(chan []int16)
	errs := make(chan error)
	close(pcm)
	close(errs)
	return pcm, errs
}
func (testRecorder) Format() audio.Format { return audio.Default() }
func (testRecorder) Describe() string     { return "test recorder" }

type testPlayer struct{}

func (testPlayer) PlayWAV(context.Context, []byte) error                { return nil }
func (testPlayer) PlayPCM(context.Context, []int16, audio.Format) error { return nil }
func (testPlayer) Stop() error                                          { return nil }
func (testPlayer) Describe() string                                     { return "test player" }

type testSegmenter struct{ count int }

func (s testSegmenter) Run(context.Context, <-chan []int16, audio.Format) <-chan vad.Utterance {
	out := make(chan vad.Utterance, s.count)
	for range s.count {
		out <- vad.Utterance{Format: audio.Default()}
	}
	close(out)
	return out
}

type queuedTranscriber struct {
	texts []string
	next  int
}

func (t *queuedTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	text := t.texts[t.next]
	t.next++
	return text, nil
}
func (*queuedTranscriber) Name() string              { return "test transcriber" }
func (*queuedTranscriber) Available() (bool, string) { return true, "available" }

type testSynthesizer struct{}

func (testSynthesizer) Synthesize(context.Context, string) ([]byte, error) { return nil, nil }
func (testSynthesizer) Name() string                                       { return "test synthesizer" }
func (testSynthesizer) Available() (bool, string)                          { return true, "available" }

type countingPlanner struct{ calls int }

func (p *countingPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	p.calls++
	return brain.Plan{}, nil
}
func (*countingPlanner) Name() string              { return "test planner" }
func (*countingPlanner) Available() (bool, string) { return true, "available" }
