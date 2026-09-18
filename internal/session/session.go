// Package session connects audio, VAD, speech, reasoning and devices into the
// wake-gated assistant loop.
package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/timer"
	"github.com/jerryfane/voice/internal/vad"
	"github.com/jerryfane/voice/internal/wake"
)

// Assistant is one configured Voice runtime.
type Assistant struct {
	Recorder audio.Recorder
	Player   audio.Player
	VAD      vad.Segmenter
	STT      speech.Transcriber
	TTS      speech.Synthesizer
	Brain    brain.Planner
	Devices  *device.Registry
	// Feedback acknowledges an accepted wake phrase locally. A nil Notifier
	// disables light and sound without changing command handling.
	Feedback *feedback.Notifier
	// Timers runs local timers. A nil Scheduler means the timer vocabulary
	// falls through to the planner, as it did before timers existed.
	Timers *timer.Scheduler
	// Missed are timers that came due while Voice was not running; Run
	// reports them once at startup.
	Missed      []timer.Timer
	WakePhrases []string
	WakeFuzz    float64
	// Now is the clock used for timer arithmetic. Nil means time.Now.
	Now    func() time.Time
	Logger *log.Logger
}

func (a *Assistant) logf(f string, v ...any) {
	if a.Logger != nil {
		a.Logger.Printf(f, v...)
	}
}

// HandleText is the non-voice entry point used by the CLI, agents and tests.
// It runs the exact same planner and device execution path as a spoken command.
func (a *Assistant) HandleText(ctx context.Context, text string) (brain.Plan, error) {
	p, err := a.Brain.Plan(ctx, strings.TrimSpace(text), a.Devices.Infos())
	if err != nil {
		return p, err
	}
	for _, act := range p.Actions {
		dev, err := a.Devices.Get(act.Device)
		if err != nil {
			return p, err
		}
		if !device.Supports(dev, act.Op) {
			return p, fmt.Errorf("%s does not support %s", dev.ID(), act.Op)
		}
		state, err := dev.Apply(ctx, device.Command{Op: act.Op, Args: act.Args})
		if err != nil {
			return p, fmt.Errorf("%s %s: %w", act.Device, act.Op, err)
		}
		a.logf("action device=%s op=%s state=%v", act.Device, act.Op, state)
	}
	return p, nil
}

// Speak synthesizes and plays one response.
func (a *Assistant) Speak(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	wav, err := a.TTS.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	return a.Player.PlayWAV(ctx, wav)
}

// Run listens until ctx is cancelled. Every model request must contain the wake
// phrase and command in the same utterance.
func (a *Assistant) Run(ctx context.Context) error {
	// Establish a known indicator state at startup and leave it idle however
	// Run exits: cancellation, shutdown or a spoken "turn off".
	a.Feedback.Restore()
	defer a.Feedback.Restore()
	// Each stage of the pipeline logs its own outcome. A device that hears
	// nothing and a device whose transcriber cannot start produced the
	// identical journal - one log line after a *successful* transcription and
	// nothing before it - which is how a broken speech engine masqueraded as a
	// dead microphone for a whole boot. Silence should now name the stage that
	// stopped: capture, segment, transcribe, or wake.
	a.logf("stage=capture state=starting device=%q", a.Recorder.Describe())
	pcm, recErr := a.Recorder.Stream(ctx)
	utterances := a.VAD.Run(ctx, pcm, a.Recorder.Format())
	var fired <-chan timer.Timer
	if a.Timers != nil {
		done := make(chan struct{})
		defer close(done)
		go a.Timers.Run(done)
		fired = a.Timers.Fired()
		a.announceMissed(ctx, a.Missed)
	}

	for {
		select {
		case <-ctx.Done():
			// Capture's cleanup runs as its goroutine unwinds: it clears the
			// speakerphone's off-hook report, which is the difference between
			// a device left lit with its microphone held open and one that is
			// idle. Returning here without waiting lets the process exit
			// first, so wait for the sample channel to close.
			a.awaitCaptureStop(pcm)
			return nil
		case err, ok := <-recErr:
			if ok && err != nil {
				a.logf("stage=capture state=failed err=%v", err)
				return err
			}
			recErr = nil
		case t := <-fired:
			a.announceFired(ctx, t)
		case u, ok := <-utterances:
			if !ok {
				a.logf("stage=segment state=closed")
				return nil
			}
			ms := len(u.PCM) * 1000 / max(u.Format.SampleRate, 1)
			a.logf("stage=segment state=ok samples=%d ms=%d peak=%.4f truncated=%v",
				len(u.PCM), ms, u.Peak, u.Truncated)
			text, err := a.STT.Transcribe(ctx, u.PCM, u.Format)
			if err != nil {
				a.logf("stage=transcribe state=failed err=%v", err)
				continue
			}
			text = strings.TrimSpace(text)
			if text == "" {
				a.logf("stage=transcribe state=empty peak=%.4f", u.Peak)
				continue
			}
			a.logf("stage=transcribe state=ok heard=%q peak=%.4f", text, u.Peak)
			matched, command, _ := wake.Match(text, a.WakePhrases, a.WakeFuzz)
			if !matched {
				a.logf("stage=wake state=nomatch")
				continue
			}
			a.logf("stage=wake state=matched command=%q", command)
			if stop := a.accepted(ctx, command); stop {
				return nil
			}
		}
	}
}

// captureStopTimeout bounds the wait for capture to unwind at shutdown. A
// stuck capture command must not keep the service from exiting; systemd's
// stop timeout would kill it anyway, and this way the delay is ours and
// explained rather than a hang.
const captureStopTimeout = 2 * time.Second

// awaitCaptureStop drains samples until the recorder closes the channel, which
// it does only after its deferred cleanup has run.
func (a *Assistant) awaitCaptureStop(pcm <-chan []int16) {
	if pcm == nil {
		return
	}
	deadline := time.NewTimer(captureStopTimeout)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-pcm:
			if !ok {
				return
			}
		case <-deadline.C:
			a.logf("capture did not stop within %s; exiting without its cleanup", captureStopTimeout)
			return
		}
	}
}

// accepted handles one utterance that passed the wake gate. All local feedback
// happens here and nowhere else, so ambient noise, an embedded mention or an
// approximate phrase can never produce a light or a sound.
func (a *Assistant) accepted(ctx context.Context, command string) (stop bool) {
	a.Feedback.Accepted(ctx)
	defer a.Feedback.Restore()
	if command == "" {
		a.speakLogged(ctx, "Please say Hey Voice followed by your request.")
		return false
	}
	if a.timerControl(ctx, command) {
		return false
	}
	if reply, stop, handled := localControl(command); handled {
		a.speakLogged(ctx, reply)
		return stop
	}
	// The planner and the speaker are timed because they were the only
	// unlogged stages, and they turned out to own almost all of a
	// minute-long interaction on the device: capture, segment, transcribe and
	// wake were all instrumented and all fast, so the slowest part of a real
	// request was the one part nobody could see.
	started := time.Now()
	p, err := a.HandleText(ctx, command)
	if err != nil {
		a.logf("stage=plan state=failed ms=%.3f err=%v", sinceMS(started), err)
		a.say(ctx, "I couldn't do that.")
		return false
	}
	a.logf("stage=plan state=ok ms=%.3f reply=%d", sinceMS(started), len(p.Speak))
	a.speakLogged(ctx, p.Speak)
	return false
}

// speakLogged times synthesis and playback together, which is what a person
// waits through, and reports the text length so a slow reply can be told from
// a long one.
func (a *Assistant) speakLogged(ctx context.Context, text string) {
	started := time.Now()
	if err := a.Speak(ctx, text); err != nil {
		a.logf("stage=speak state=failed ms=%.3f chars=%d err=%v", sinceMS(started), len(text), err)
		return
	}
	a.logf("stage=speak state=ok ms=%.3f chars=%d", sinceMS(started), len(text))
}

// sinceMS reports elapsed milliseconds with fractions. Milliseconds() alone
// truncates toward zero, so a sub-millisecond stage logged ms=0, which is
// indistinguishable at a glance from a stage that was never measured - the
// exact ambiguity this logging exists to remove.
func sinceMS(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

func localControl(command string) (reply string, stop bool, handled bool) {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "turn off", "turn yourself off", "stop listening", "go offline":
		return "Turning off.", true, true
	case "turn on", "start listening", "go online":
		return "I'm already on.", false, true
	default:
		return "", false, false
	}
}
