package session

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/proposal"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/timer"
	"github.com/jerryfane/voice/internal/vad"
	"github.com/jerryfane/voice/internal/wake"
)

// Build wires a complete assistant from config. Feedback hardware that is
// missing or unsupported is reported through the assistant's logger and
// degrades to no light: it must never stop Voice from accepting commands.
func Build(c config.Config) *Assistant {
	logger := log.New(os.Stderr, "voice: ", log.LstdFlags)
	f := audio.Format{SampleRate: c.Input.SampleRate, Channels: 1}
	frame := f.SampleRate / 50
	rec, player, light, boundary := buildAudio(c, f, frame, logger)
	vocabulary := speech.NewVocabulary(c.STT.Vocabulary)
	commandSTT := speech.NewCommandTranscriber(c.STT.Name, c.STT.Command, c.STT.Model, c.STT.Timeout.D(), faults.Log(logger, "speech-to-text"))
	commandSTT.Vocabulary = vocabulary.Prompt()
	var localSTT speech.Transcriber = commandSTT
	if resident := c.STT.Resident; resident != nil {
		residentSTT := speech.NewWhisperServerTranscriber(resident.Endpoint, resident.Timeout.D())
		residentSTT.Vocabulary = vocabulary.Prompt()
		localSTT = speech.NewCascade(logger,
			residentSTT,
			commandSTT,
		)
	}
	stt := localSTT
	var wakeSTT speech.Transcriber
	if remote := c.STT.OpenRouter; remote != nil {
		key := os.Getenv(remote.APIKeyEnv)
		if key != "" {
			if c.Wake.Detector == "stt" {
				wakeSTT = localSTT
			}
			stt = speech.NewCascade(logger,
				speech.NewOpenRouterTranscriber(remote.Endpoint, remote.Model, key, remote.Timeout.D(), logger),
				localSTT,
			)
		}
	}
	localTTS := speech.NewCommandSynthesizer(c.TTS.Name, c.TTS.Command, c.TTS.Model, c.TTS.Timeout.D(), faults.Log(logger, "text-to-speech"))
	var tts speech.Synthesizer = localTTS
	if remote := c.TTS.OpenRouter; remote != nil {
		tts = speech.NewSynthesizerCascade(logger,
			speech.NewOpenRouterSynthesizer(remote.Endpoint, remote.Model, remote.Voice, os.Getenv(remote.APIKeyEnv), remote.Timeout.D(), logger),
			localTTS,
		)
	}
	ext := brain.NewExternal("brain", c.Brain.Command, c.Brain.Persona, c.Brain.Timeout.D(), c.Brain.Mode)
	var planner brain.Planner = ext
	if jev := c.Brain.Jev; jev != nil {
		planner = brain.NewJev(jev.Endpoint, jev.Model, os.Getenv(jev.APIKeyEnv), jev.Confidence, jev.Timeout.D(), planner, logger)
	}
	if c.Brain.Rules {
		planner = brain.NewRules(planner)
	}
	var front brain.FrontDesk
	if configured := c.Brain.Front; configured != nil {
		if key := os.Getenv(configured.APIKeyEnv); key != "" {
			front = brain.NewOpenRouterFrontDesk(configured.Endpoint, configured.Model, key, configured.Timeout.D(), logger)
		}
	}
	frames := func(d config.Duration) int {
		n := int(d.D().Seconds() * float64(f.SampleRate) / float64(frame))
		if n < 1 {
			n = 1
		}
		return n
	}
	// A path loads a file; a bare name renders a built-in. Validate has
	// already proved whichever it is can be produced, so a failure here means
	// a config assembled in code rather than loaded from disk.
	sound, err := audio.Earcon(c.Feedback.Sound, f, c.Feedback.Volume)
	if audio.IsSoundFile(c.Feedback.Sound) {
		sound, err = audio.SoundFromFile(c.Feedback.Sound, f, c.Feedback.Volume, audio.AcknowledgementSound)
	}
	if err != nil {
		logger.Printf("wake sound: %v", err)
	}
	sound = audio.PrependPCMSilence(sound, f, playbackLeadIn)
	// The thinking sound is rendered at startup like the acknowledgement, so
	// a config typo is reported once here rather than discovered mid-answer.
	// Same rule as the acknowledgement: a path loads a file, a bare name
	// renders a built-in.
	working, err := audio.Thinking(c.Feedback.Thinking, f, c.Feedback.ResolvedThinkingVolume())
	if audio.IsSoundFile(c.Feedback.Thinking) {
		working, err = audio.SoundFromFile(c.Feedback.Thinking, f, c.Feedback.ResolvedThinkingVolume(), audio.ThinkingSound)
	}
	if err != nil {
		logger.Printf("thinking sound: %v", err)
	}
	fb := &feedback.Notifier{Indicator: light, Player: player, Sound: sound, Working: working, ThinkingDelay: c.Feedback.ThinkingDelay.D(), Format: f, Logger: logger}
	params := vad.Params{Threshold: c.Wake.VAD.Threshold, MinSpeech: frames(c.Wake.VAD.MinSpeech), Silence: frames(c.Wake.VAD.Silence), MaxUtterance: frames(c.Wake.VAD.MaxUtterance), PreRoll: frames(c.Wake.VAD.PreRoll), FrameSize: frame}
	var seg vad.Segmenter = vad.NewEnergy(params)
	if c.Wake.Detector == "sherpa" && c.Wake.Sherpa != nil {
		s := c.Wake.Sherpa
		detector := wake.NewSherpa(wake.SherpaConfig{
			Encoder:           s.Encoder,
			Decoder:           s.Decoder,
			Joiner:            s.Joiner,
			Tokens:            s.Tokens,
			Keywords:          s.Keywords,
			NumThreads:        s.NumThreads,
			MaxActivePaths:    s.MaxActivePaths,
			KeywordsScore:     float32(s.KeywordsScore),
			KeywordsThreshold: float32(s.KeywordsThreshold),
		})
		keyword := vad.NewKeyword(params, detector, logger)
		keyword.SetFollowUp(frames(c.Wake.FollowUpTimeout), func(ctx context.Context) { fb.Accepted(ctx) }, fb.Restore)
		// Keep at least 1.2 seconds of actual listening AFTER the playback
		// tail, not a short gap mostly consumed by the guard. Longer padding
		// in a configured sound still sets the cadence. Once speech begins,
		// CanThink holds the next pulse through candidate and queued speech;
		// this does not extend the session's absolute authorization deadline.
		audible := audio.TrimTrailingSilence(fb.Working)
		if len(audible) > 0 {
			fb.ThinkingGap = max(playbackTail+1200*time.Millisecond, time.Duration(len(fb.Working)-len(audible))*time.Second/time.Duration(f.SampleRate*f.Channels))
			fb.Working = audio.PrependPCMSilence(audible, f, playbackLeadIn)
		}
		fb.BeforeThinking = keyword.Suspend
		fb.AfterThinking = func() { keyword.ResumeAfter(playbackTail) }
		fb.CanThink = keyword.CanThink
		seg = keyword
	}
	if boundary != nil {
		seg.(interface{ SetCaptureBoundary(*audio.CaptureBoundary) }).SetCaptureBoundary(boundary)
	}
	var timers *timer.Scheduler
	var missed []timer.Timer
	if c.Timers.Enabled {
		path := c.Timers.File
		if path == "" {
			p, err := config.StatePath("timers.db")
			if err != nil {
				logger.Printf("timers: %v", err)
			}
			path = p
		}
		store, err := timer.Open(path)
		if err != nil {
			// Without a store the timer vocabulary would accept requests it
			// cannot keep, so say why and leave it to the planner.
			logger.Printf("timers disabled: %v", err)
		} else {
			timers, missed, err = timer.New(store, faults.Log(logger, "timers"))
			if err != nil {
				// A scheduler is still returned: unreadable saved state costs
				// the old timers, not the ability to set new ones.
				logger.Printf("timers: %v", err)
			}
		}
	}
	var proposals *proposal.Store
	if c.Proposals.Enabled {
		path := c.Proposals.File
		if path == "" {
			p, err := config.StatePath("proposals.db")
			if err != nil {
				logger.Printf("proposals: %v", err)
			}
			path = p
		}
		store, err := proposal.Open(path)
		if err != nil {
			// Without a store the agent would answer a request it cannot do
			// and nothing would remember it was ever asked for, which is the
			// exact loss this feature exists to prevent: say so in the
			// journal rather than fail silently.
			logger.Printf("proposals disabled: %v", err)
		} else {
			proposals = store
		}
	}
	return &Assistant{Recorder: rec, Player: player, VAD: seg, WakeSTT: wakeSTT, STT: stt, Vocabulary: vocabulary, TTS: tts, Brain: planner, Front: front, Devices: device.Build(c), Feedback: fb, Timers: timers, Missed: missed, Proposals: proposals, WakePhrases: c.Wake.Phrases, WakeFuzz: c.Wake.Fuzz, InFlightWindow: c.Wake.InFlightWindow.D(), InFlightTurns: c.Wake.InFlightTurns, ConversationWindow: c.Wake.ConversationWindow.D(), ConversationTurns: c.Wake.ConversationTurns, Logger: logger}
}
