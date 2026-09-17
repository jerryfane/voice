// Package session connects audio, VAD, speech, reasoning and devices into the
// wake-gated assistant loop.
package session

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/vad"
	"github.com/jerryfane/voice/internal/wake"
)

// Assistant is one configured Voice runtime.
type Assistant struct {
	Recorder    audio.Recorder
	Player      audio.Player
	VAD         vad.Segmenter
	STT         speech.Transcriber
	TTS         speech.Synthesizer
	Brain       brain.Planner
	Devices     *device.Registry
	WakePhrases []string
	WakeFuzz    float64
	Logger      *log.Logger
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
	pcm, recErr := a.Recorder.Stream(ctx)
	utterances := a.VAD.Run(ctx, pcm, a.Recorder.Format())

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-recErr:
			if ok && err != nil {
				return err
			}
			recErr = nil
		case u, ok := <-utterances:
			if !ok {
				return nil
			}
			text, err := a.STT.Transcribe(ctx, u.PCM, u.Format)
			if err != nil {
				a.logf("transcribe: %v", err)
				continue
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			a.logf("heard=%q peak=%.4f", text, u.Peak)
			matched, command, _ := wake.Match(text, a.WakePhrases, a.WakeFuzz)
			if !matched {
				continue
			}
			if command == "" {
				if err := a.Speak(ctx, "Please say Hey Voice followed by your request."); err != nil {
					a.logf("speak: %v", err)
				}
				continue
			}
			if reply, stop, handled := localControl(command); handled {
				if err := a.Speak(ctx, reply); err != nil {
					a.logf("speak: %v", err)
				}
				if stop {
					return nil
				}
				continue
			}
			p, err := a.HandleText(ctx, command)
			if err != nil {
				a.logf("command %q: %v", command, err)
				_ = a.Speak(ctx, "I couldn't do that.")
				continue
			}
			if err := a.Speak(ctx, p.Speak); err != nil {
				a.logf("speak: %v", err)
			}
		}
	}
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
