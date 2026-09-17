package feedback

import (
	"context"
	"errors"
	"testing"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/hid"
)

// logIndicator and logPlayer share one event log so a test can assert the
// order of light and sound, not just that both happened.
type logIndicator struct {
	log *[]string
	err error
}

func (l *logIndicator) Set(s State) error {
	*l.log = append(*l.log, "light:"+s.String())
	return l.err
}
func (*logIndicator) Describe() string { return "log indicator" }

type logPlayer struct {
	log *[]string
	err error
}

func (p *logPlayer) PlayWAV(context.Context, []byte) error { return nil }
func (p *logPlayer) PlayPCM(context.Context, []int16, audio.Format) error {
	*p.log = append(*p.log, "sound")
	return p.err
}
func (*logPlayer) Stop() error      { return nil }
func (*logPlayer) Describe() string { return "log player" }

func notifier(log *[]string, indErr, playErr error) *Notifier {
	return &Notifier{
		Indicator: &logIndicator{log: log, err: indErr},
		Player:    &logPlayer{log: log, err: playErr},
		Sound:     []int16{1, 2, 3},
		Format:    audio.Default(),
	}
}

// The light is what the user is looking at, so it must not wait behind a few
// hundred milliseconds of audio playback.
func TestLightChangesBeforeSoundPlays(t *testing.T) {
	var log []string
	notifier(&log, nil, nil).Accepted(context.Background())
	want := []string{"light:listening", "sound"}
	if len(log) != len(want) || log[0] != want[0] || log[1] != want[1] {
		t.Fatalf("feedback order = %v, want %v", log, want)
	}
}

// Hardware that rejects the write must not cost the user their command, and
// must not swallow the acknowledgement sound either.
func TestFailingIndicatorStillPlaysSound(t *testing.T) {
	var log []string
	notifier(&log, errors.New("hidraw: permission denied"), nil).Accepted(context.Background())
	if len(log) != 2 || log[1] != "sound" {
		t.Fatalf("feedback = %v, want the sound to play despite the light failing", log)
	}
}

func TestRestoreNeverPlaysSound(t *testing.T) {
	var log []string
	n := notifier(&log, nil, nil)
	n.Restore()
	if len(log) != 1 || log[0] != "light:idle" {
		t.Fatalf("restore produced %v, want a single idle light change", log)
	}
}

func TestNilNotifierDoesNothing(t *testing.T) {
	var n *Notifier
	n.Accepted(context.Background())
	n.Restore()
	if got := n.Describe(); got != "disabled" {
		t.Fatalf("Describe() = %q, want %q", got, "disabled")
	}
}

// fakeDevice advertises a chosen set of LED usages.
type fakeDevice struct {
	usages map[uint16]bool
	on     map[uint16]bool
}

func (d *fakeDevice) Has(page, usage uint16) bool {
	return page == hid.PageLED && d.usages[usage]
}
func (d *fakeDevice) Set(page, usage uint16, on bool) (bool, error) {
	if !d.Has(page, usage) {
		return false, nil
	}
	if d.on == nil {
		d.on = map[uint16]bool{}
	}
	d.on[usage] = on
	return true, nil
}
func (*fakeDevice) Describe() string { return "fake speakerphone" }

func TestAutoLightPrefersMicrophoneLED(t *testing.T) {
	dev := &fakeDevice{usages: map[uint16]bool{hid.LEDOffHook: true, hid.LEDRing: true, hid.LEDMic: true}}
	ind, why := NewLight(dev, LightAuto)
	if err := ind.Set(Listening); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !dev.on[hid.LEDMic] {
		t.Fatalf("auto chose %q and did not light the microphone LED (state %v)", why, dev.on)
	}
	if dev.on[hid.LEDOffHook] {
		t.Error("the listening light drove the off-hook LED, which capture holds for the whole session")
	}
}

func TestAutoLightFallsBackToRingWhenNoMicLED(t *testing.T) {
	dev := &fakeDevice{usages: map[uint16]bool{hid.LEDRing: true, hid.LEDMute: true}}
	ind, _ := NewLight(dev, LightAuto)
	if err := ind.Set(Listening); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !dev.on[hid.LEDRing] {
		t.Fatalf("expected the ring LED, got %v", dev.on)
	}
}

func TestUnsupportedHardwareDegradesToNoLight(t *testing.T) {
	for name, dev := range map[string]Device{
		"no device":        nil,
		"no LED outputs":   &fakeDevice{usages: map[uint16]bool{}},
		"only off-hook":    &fakeDevice{usages: map[uint16]bool{hid.LEDOffHook: true}},
		"typed nil device": (*hid.Telephony)(nil),
	} {
		ind, why := NewLight(dev, LightAuto)
		if _, ok := ind.(Nop); !ok {
			t.Errorf("%s: got %T (%s), want Nop", name, ind, why)
		}
		if err := ind.Set(Listening); err != nil {
			t.Errorf("%s: Nop.Set returned %v", name, err)
		}
	}
}

func TestExplicitLightRejectsUnadvertisedUsage(t *testing.T) {
	dev := &fakeDevice{usages: map[uint16]bool{hid.LEDRing: true}}
	ind, why := NewLight(dev, LightMic)
	if _, ok := ind.(Nop); !ok {
		t.Fatalf("got %T, want Nop when the device lacks the requested LED", ind)
	}
	if why == "" {
		t.Error("no explanation given for the disabled light")
	}
}
