package feedback

import (
	"fmt"
	"strings"

	"github.com/jerryfane/voice/internal/hid"
)

// Lights are the selectable indicators for config feedback.light.
const (
	LightAuto = "auto"
	LightNone = "none"
	LightMic  = "mic"
	LightRing = "ring"
	LightMute = "mute"
	LightHold = "hold"
)

// Lights lists the accepted feedback.light values.
func Lights() []string {
	return []string{LightAuto, LightNone, LightMic, LightRing, LightMute, LightHold}
}

// led names one LED-page usage a speakerphone may advertise.
type led struct {
	name  string
	usage uint16
}

// leds maps config names to LED usages. The off-hook LED is deliberately
// absent: capture holds it for the entire session to keep the microphone
// un-gated, so it can never change state to mean "listening".
var leds = map[string]led{
	LightMic:  {"microphone LED", hid.LEDMic},
	LightRing: {"ring LED", hid.LEDRing},
	LightMute: {"mute LED", hid.LEDMute},
	LightHold: {"hold LED", hid.LEDHold},
}

// autoOrder is the preference when feedback.light is "auto": the microphone
// LED is the closest thing to "Voice is listening", then the ring, then the
// remaining indicators.
var autoOrder = []string{LightMic, LightRing, LightMute, LightHold}

// Device is the part of hid.Telephony the light needs: capability discovery
// and a write that preserves the rest of the shared output report. Its methods
// are nil-safe on *hid.Telephony, so an unconfigured device needs no branch.
type Device interface {
	Has(page, usage uint16) bool
	Set(page, usage uint16, on bool) (bool, error)
	Describe() string
}

// TelephonyLight shows the listening state on a USB speakerphone LED. Writes
// go through hid.Telephony, which keeps the rest of the shared output report
// intact, so lighting up never clears the off-hook bit that capture depends on.
type TelephonyLight struct {
	dev   Device
	name  string
	usage uint16
}

// NewLight resolves the configured choice against what the device actually
// advertises. An unsupported device or usage yields Nop, so Voice keeps
// accepting commands without a light instead of failing to start; the returned
// string explains what was chosen for `voice doctor`.
func NewLight(dev Device, choice string) (Indicator, string) {
	choice = strings.ToLower(strings.TrimSpace(choice))
	if choice == "" {
		choice = LightAuto
	}
	if choice == LightNone {
		return Nop{}, "light disabled by config"
	}
	if dev == nil {
		return Nop{}, "no telephony HID device configured"
	}
	if choice == LightAuto {
		for _, name := range autoOrder {
			l := leds[name]
			if dev.Has(hid.PageLED, l.usage) {
				return &TelephonyLight{dev: dev, name: l.name, usage: l.usage}, fmt.Sprintf("%s (auto) on %s", l.name, dev.Describe())
			}
		}
		return Nop{}, "device advertises no usable indicator LED: " + dev.Describe()
	}
	l, ok := leds[choice]
	if !ok {
		return Nop{}, fmt.Sprintf("unknown light %q (choose one of %s)", choice, strings.Join(Lights(), ", "))
	}
	if !dev.Has(hid.PageLED, l.usage) {
		return Nop{}, fmt.Sprintf("device does not advertise the %s: %s", l.name, dev.Describe())
	}
	return &TelephonyLight{dev: dev, name: l.name, usage: l.usage}, l.name + " on " + dev.Describe()
}

func (t *TelephonyLight) Set(s State) error {
	ok, err := t.dev.Set(hid.PageLED, t.usage, s == Listening)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not an advertised output", t.name)
	}
	return nil
}

func (t *TelephonyLight) Describe() string { return t.name + " on " + t.dev.Describe() }
