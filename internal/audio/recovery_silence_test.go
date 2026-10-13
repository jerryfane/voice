package audio

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestRecoveryAcceptsDigitalSilenceWithoutLosingNextSpeech(t *testing.T) {
	primary := newRecoveryRecorder("noise-gated USB")
	fallback := newRecoveryRecorder("fallback")
	opts := recoveryOptions()
	opts.ZeroTimeout = 0
	r := makeRecovery(t, []*recoveryRecorder{primary, fallback}, opts, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	pcm, errs := r.Stream(ctx)
	defer endRecovery(t, cancel, pcm, errs, primary, fallback)
	capture := waitCapture(t, primary)
	// A noise gate may emit genuine all-zero PCM before and between speech.
	// It must not withhold the input or restart the source during that silence.
	for range 50 {
		sendCapture(t, capture, pcmFrame(0))
		if frame := nextPCM(t, pcm); len(frame) != 320 || !slices.Equal(frame, pcmFrame(0)) {
			t.Fatal("valid digital silence was replaced by a discontinuity")
		}
	}
	speech := pcmFrame(2000)
	sendCapture(t, capture, speech)
	if !slices.Equal(nextPCM(t, pcm), speech) {
		t.Fatal("speech after silence was discarded or altered")
	}
	// Disabling the signal watchdog must not disable actual transport recovery.
	capture.fail <- errors.New("device disconnected")
	if frame := nextPCM(t, pcm); frame != nil {
		t.Fatal("real source loss did not publish a discontinuity")
	}
	next := waitCapture(t, fallback)
	sendCapture(t, next, pcmFrame(123))
	if !slices.Equal(nextPCM(t, pcm), pcmFrame(123)) {
		t.Fatal("fallback audio missing after actual source loss")
	}
}
