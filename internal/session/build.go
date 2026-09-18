package session

import (
	"log"
	"os"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/hid"
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
	var tel *hid.Telephony
	if c.Input.TelephonyHID != "" {
		var err error
		tel, err = hid.Open(c.Input.TelephonyHID)
		if err != nil {
			// Open still returns a usable handle for the standard report when
			// only the descriptor was unreadable.
			logger.Printf("telephony HID: %v", err)
		}
		// Every report write is logged with its exact bytes and length. A
		// light that does not come on was otherwise indistinguishable from a
		// write that never happened: on the device, the raw report 02 09 00
		// lit the ring while Voice's own path lit nothing and reported no
		// error, and nothing in the journal could say which bytes it sent.
		tel.SetLogger(logger.Printf)
		// A ready line, so the absence of written lines MEANS something. The
		// review pointed out that a successful-write log cannot by itself
		// separate "wrote the wrong bytes" from "never wrote at all" - that
		// second case is only visible in the wake-light fallback line and the
		// feedback error line, which an engineer has to know to correlate.
		// With this, a ready line and no written lines is the answer on its
		// own.
		tel.LogReady()
	}
	rec := audio.NewCommandRecorder(c.Input.Command, c.Input.Device, f, frame, tel, faults.Log(logger, "capture"))
	player := audio.NewCommandPlayer(c.Output.Command, c.Output.Device)
	commandSTT := speech.NewCommandTranscriber(c.STT.Name, c.STT.Command, c.STT.Model, c.STT.Timeout.D(), faults.Log(logger, "speech-to-text"))
	var localSTT speech.Transcriber = commandSTT
	if resident := c.STT.Resident; resident != nil {
		localSTT = speech.NewCascade(logger,
			speech.NewWhisperServerTranscriber(resident.Endpoint, resident.Timeout.D()),
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
	tts := speech.NewCommandSynthesizer(c.TTS.Name, c.TTS.Command, c.TTS.Model, c.TTS.Timeout.D(), faults.Log(logger, "text-to-speech"))
	ext := brain.NewExternal("brain", c.Brain.Command, c.Brain.Persona, c.Brain.Timeout.D(), c.Brain.Mode)
	var planner brain.Planner = ext
	if jev := c.Brain.Jev; jev != nil {
		planner = brain.NewJev(jev.Endpoint, jev.Model, os.Getenv(jev.APIKeyEnv), jev.Confidence, jev.Timeout.D(), planner, logger)
	}
	if c.Brain.Rules {
		planner = brain.NewRules(planner)
	}
	frames := func(d config.Duration) int {
		n := int(d.D().Seconds() * float64(f.SampleRate) / float64(frame))
		if n < 1 {
			n = 1
		}
		return n
	}
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
		seg = vad.NewKeyword(params, detector, logger)
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
	light, why := feedback.NewLight(tel, c.Feedback.Light)
	if _, off := light.(feedback.Nop); off && c.Feedback.Light != feedback.LightNone {
		logger.Printf("wake light: %s", why)
	}
	sound, err := audio.Earcon(c.Feedback.Sound, f, c.Feedback.Volume)
	if err != nil {
		// Validate rejects unknown names, so this only happens for a config
		// built in code.
		logger.Printf("wake sound: %v", err)
	}
	// The thinking sound is rendered at startup like the acknowledgement, so
	// a config typo is reported once here rather than discovered mid-answer.
	working, err := audio.Thinking(c.Feedback.Thinking, f, c.Feedback.ResolvedThinkingVolume())
	if err != nil {
		logger.Printf("thinking sound: %v", err)
	}
	fb := &feedback.Notifier{Indicator: light, Player: player, Sound: sound, Working: working, Format: f, Logger: logger}
	return &Assistant{Recorder: rec, Player: player, VAD: seg, WakeSTT: wakeSTT, STT: stt, TTS: tts, Brain: planner, Devices: device.Build(c), Feedback: fb, Timers: timers, Missed: missed, WakePhrases: c.Wake.Phrases, WakeFuzz: c.Wake.Fuzz, Logger: logger}
}
