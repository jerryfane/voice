package session

import (
	"log"
	"os"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/hid"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/timer"
	"github.com/jerryfane/voice/internal/vad"
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
	}
	rec := audio.NewCommandRecorder(c.Input.Command, c.Input.Device, f, frame, tel)
	player := audio.NewCommandPlayer(c.Output.Command, c.Output.Device)
	stt := speech.NewCommandTranscriber(c.STT.Name, c.STT.Command, c.STT.Model, c.STT.Timeout.D())
	tts := speech.NewCommandSynthesizer(c.TTS.Name, c.TTS.Command, c.TTS.Model, c.TTS.Timeout.D())
	ext := brain.NewExternal("brain", c.Brain.Command, c.Brain.Persona, c.Brain.Timeout.D(), c.Brain.Mode)
	var planner brain.Planner = ext
	if c.Brain.Rules {
		planner = brain.NewRules(ext)
	}
	frames := func(d config.Duration) int {
		n := int(d.D().Seconds() * float64(f.SampleRate) / float64(frame))
		if n < 1 {
			n = 1
		}
		return n
	}
	seg := vad.NewEnergy(vad.Params{Threshold: c.Wake.VAD.Threshold, MinSpeech: frames(c.Wake.VAD.MinSpeech), Silence: frames(c.Wake.VAD.Silence), MaxUtterance: frames(c.Wake.VAD.MaxUtterance), PreRoll: frames(c.Wake.VAD.PreRoll), FrameSize: frame})
	var timers *timer.Scheduler
	var missed []timer.Timer
	if c.Timers.Enabled {
		path := c.Timers.File
		if path == "" {
			p, err := config.StatePath("timers.json")
			if err != nil {
				logger.Printf("timers: %v", err)
			}
			path = p
		}
		var err error
		timers, missed, err = timer.New(timer.Store{Path: path})
		if err != nil {
			// A scheduler is still returned: unreadable saved state costs the
			// old timers, not the ability to set new ones.
			logger.Printf("timers: %v", err)
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
	fb := &feedback.Notifier{Indicator: light, Player: player, Sound: sound, Format: f, Logger: logger}
	return &Assistant{Recorder: rec, Player: player, VAD: seg, STT: stt, TTS: tts, Brain: planner, Devices: device.Build(c), Feedback: fb, Timers: timers, Missed: missed, WakePhrases: c.Wake.Phrases, WakeFuzz: c.Wake.Fuzz, Logger: logger}
}
