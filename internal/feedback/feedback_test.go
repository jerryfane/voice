package feedback

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// The thinking loop must keep playing while the planner works, and must be
// FINISHED - not merely cancelled - by the time stop returns. A cancelled loop
// with a buffer still playing would talk over the spoken answer, and real
// playback does not stop mid-buffer just because a context was cancelled.
//
// The first version of this test asserted only that no NEW plays started
// after stop, which a stop that does not wait also satisfies: the mutant
// passed. Asserting that nothing is in flight is what distinguishes them.
func TestThinkingIsFinishedNotMerelyCancelledWhenStopReturns(t *testing.T) {
	p := &loopPlayer{}
	n := &Notifier{Player: p, Working: []int16{1, 2, 3}, Format: audio.Format{SampleRate: 16000, Channels: 1}}

	stop := n.Thinking(context.Background())
	for i := 0; i < 500 && p.plays() < 2; i++ {
		time.Sleep(time.Millisecond)
	}
	if p.plays() < 2 {
		t.Fatalf("played %d times, expected the sound to loop", p.plays())
	}

	stop()
	if inFlight := p.inFlight(); inFlight != 0 {
		t.Errorf("%d playback(s) still in flight when stop returned; the loop would overlap the reply", inFlight)
	}
	settled := p.plays()
	time.Sleep(20 * time.Millisecond)
	if p.plays() != settled {
		t.Errorf("played %d more times after stop returned", p.plays()-settled)
	}
}

// A nil notifier, a nil player and no sound must all be safe, so callers need
// no branch and can defer the stop unconditionally.
func TestThinkingIsSafeWithNothingConfigured(t *testing.T) {
	var none *Notifier
	none.Thinking(context.Background())()

	(&Notifier{}).Thinking(context.Background())()
	(&Notifier{Player: &loopPlayer{}}).Thinking(context.Background())()
	(&Notifier{Working: []int16{1}}).Thinking(context.Background())()
}

// loopPlayer counts plays under a mutex so the test can observe a goroutine
// looping without racing it.
type loopPlayer struct {
	mu   sync.Mutex
	n    int
	busy int
}

func (p *loopPlayer) PlayWAV(context.Context, []byte) error { return nil }
func (p *loopPlayer) PlayPCM(ctx context.Context, _ []int16, _ audio.Format) error {
	p.mu.Lock()
	p.n++
	p.busy++
	p.mu.Unlock()
	// A real player writes a whole buffer to a device or subprocess and does
	// not abandon it the instant a context is cancelled, so this does not
	// either. That is what makes "finished" different from "cancelled".
	time.Sleep(15 * time.Millisecond)
	p.mu.Lock()
	p.busy--
	p.mu.Unlock()
	return ctx.Err()
}

func (p *loopPlayer) inFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy
}
func (*loopPlayer) Stop() error      { return nil }
func (*loopPlayer) Describe() string { return "loop player" }

func (p *loopPlayer) plays() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// A fast answer must be SILENT BY CONSTRUCTION, not by winning a race. The
// review drove 500 fast-path answers against the first version and the sound
// fired on one of them; on real hardware that means spawning and killing a
// playback subprocess for a query answered in microseconds.
//
// This asserts the delay exists rather than hoping the scheduler cancels in
// time: it holds the loop open for a fraction of the delay, which is already
// many playback cycles, and requires silence. Counting immediate start/stop
// pairs - which my first attempt did - cannot distinguish a delay from a
// lucky cancel, and the no-delay mutant passed it.
func TestThinkingStaysSilentForAFastAnswer(t *testing.T) {
	p := &loopPlayer{}
	n := &Notifier{Player: p, Working: []int16{1, 2, 3}, Format: audio.Format{SampleRate: 16000, Channels: 1}}

	stop := n.Thinking(context.Background())
	// A quarter of the delay: long enough for several playback cycles if the
	// loop had started, far short of when it is allowed to.
	time.Sleep(thinkingDelay / 4)
	stop()
	if got := p.plays(); got != 0 {
		t.Errorf("the thinking sound played %d times within %v; a fast answer must be silent by construction", got, thinkingDelay/4)
	}

	// And the immediate case, which is what the local fast path actually does.
	for i := 0; i < 200; i++ {
		n.Thinking(context.Background())()
	}
	if got := p.plays(); got != 0 {
		t.Errorf("the thinking sound played %d times across 200 immediate answers", got)
	}
}

// And a slow answer must still be covered, or the feature does nothing.
func TestThinkingPlaysForASlowAnswer(t *testing.T) {
	p := &loopPlayer{}
	n := &Notifier{Player: p, Working: []int16{1, 2, 3}, Format: audio.Format{SampleRate: 16000, Channels: 1}}

	stop := n.Thinking(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for p.plays() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if p.plays() == 0 {
		t.Error("the thinking sound never played for a slow answer, so the wait is silent again")
	}
}
