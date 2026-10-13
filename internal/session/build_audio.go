package session

import (
	"errors"
	"log"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/hid"
)

func buildAudio(c config.Config, f audio.Format, frame int, logger *log.Logger) (audio.Recorder, audio.Player, feedback.Indicator, *audio.CaptureBoundary) {
	recovering := c.Input.RecoveryEnabled()
	openHID := func(path string) *hid.Telephony {
		if path == "" {
			return nil
		}
		var tel *hid.Telephony
		var err error
		if recovering {
			tel, err = hid.NewReconnecting(path)
		} else {
			tel, err = hid.Open(path)
		}
		if err != nil {
			logger.Printf("telephony HID: %v", err)
		}
		tel.SetLogger(logger.Printf)
		var warning *hid.DescriptorWarning
		if err == nil || errors.As(err, &warning) {
			tel.LogReady()
		}
		return tel
	}
	tel := openHID(c.Input.TelephonyHID)
	primary := audio.NewCommandRecorder(c.Input.Command, c.Input.Device, f, frame, tel, faults.Log(logger, "capture"))
	primaryPlayer := audio.NewCommandPlayer(c.Output.Command, c.Output.Device)
	if !recovering {
		light, why := feedback.NewLight(tel, c.Feedback.Light)
		if _, off := light.(feedback.Nop); off && c.Feedback.Light != feedback.LightNone {
			logger.Printf("wake light: %s", why)
		}
		return primary, audio.NewSerialPlayer(primaryPlayer), light, nil
	}
	group := func(path, device string) string {
		if path != "" {
			return "hid:" + path
		}
		return "device:" + device
	}
	primary.ReopenHID = true
	choices := []audio.CaptureChoice{{Recorder: primary, Group: group(c.Input.TelephonyHID, c.Input.Device)}}
	players := []audio.Player{primaryPlayer}
	handles := []*hid.Telephony{tel}
	for _, fallback := range c.Input.Fallbacks {
		handle := openHID(fallback.TelephonyHID)
		recorder := audio.NewCommandRecorder(fallback.Command, fallback.Device, f, frame, handle, faults.Log(logger, "capture"))
		recorder.ReopenHID = true
		choices = append(choices, audio.CaptureChoice{Recorder: recorder, Group: group(fallback.TelephonyHID, fallback.Device)})
		handles = append(handles, handle)
		var player audio.Player = primaryPlayer
		if output := fallback.Output; output != nil {
			player = audio.NewCommandPlayer(output.Command, output.Device)
		}
		players = append(players, player)
	}
	routed := audio.NewRoutedPlayer(players)
	light := feedback.NewReconnectingLight(tel, c.Feedback.Light)
	boundary := audio.NewCaptureBoundary()
	options := c.Input.RecoveryOptions()
	if quiet := c.Wake.VAD.Silence.D(); quiet > options.ReturnQuiet {
		options.ReturnQuiet = quiet
	}
	// Prefer a conservative fixed low threshold over adaptive noise: a noisy
	// recovered microphone delays return rather than cutting quiet speech.
	if threshold := c.Wake.VAD.Threshold; threshold > 0 && threshold < options.ReturnThreshold {
		options.ReturnThreshold = threshold
	}
	recorder, err := audio.NewRecoveringRecorder(choices, options, boundary, logger.Printf, func(index int) {
		routed.Select(index)
		light.Select(handles[index])
	})
	if err != nil {
		// Build's caller must Validate before constructing runtime engines.
		panic("invalid capture recovery configuration: " + err.Error())
	}
	return recorder, audio.NewSerialPlayer(routed), light, boundary
}
