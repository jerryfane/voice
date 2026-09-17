package session

import (
	"log"
	"os"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/vad"
)

// Build wires a complete assistant from config.
func Build(c config.Config) *Assistant {
	f := audio.Format{SampleRate: c.Input.SampleRate, Channels: 1}
	frame := f.SampleRate / 50
	rec := audio.NewCommandRecorder(c.Input.Command, c.Input.Device, f, frame, c.Input.TelephonyHID)
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
	return &Assistant{Recorder: rec, Player: player, VAD: seg, STT: stt, TTS: tts, Brain: planner, Devices: device.Build(c), WakePhrases: c.Wake.Phrases, WakeFuzz: c.Wake.Fuzz, FollowUp: c.Wake.FollowUp.D(), Logger: log.New(os.Stderr, "voice: ", log.LstdFlags)}
}
