package hid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// powerConfDescriptor is the HID report descriptor of the Anker PowerConf
// (USB 291a:3301) installed on the Pi, read from
// /sys/class/hidraw/hidraw5/device/report_descriptor (191 bytes). Report 2
// carries the telephony LEDs Voice drives, in a two-byte payload.
var powerConfDescriptor = []byte{
	0x05, 0x0c, 0x09, 0x01, 0xa1, 0x01, 0x85, 0x01, 0x15, 0x00, 0x25, 0x01,
	0x09, 0xe9, 0x09, 0xea, 0x09, 0xe2, 0x09, 0xcd, 0x09, 0xb5, 0x09, 0xb6,
	0x09, 0xb3, 0x09, 0xb7, 0x75, 0x01, 0x95, 0x08, 0x81, 0x42, 0xc0, 0x05,
	0x0b, 0x09, 0x05, 0xa1, 0x01, 0x85, 0x02, 0x05, 0x0b, 0x15, 0x00, 0x25,
	0x01, 0x09, 0x20, 0x09, 0x97, 0x75, 0x01, 0x95, 0x02, 0x81, 0x23, 0x09,
	0x2f, 0x09, 0x21, 0x09, 0x70, 0x09, 0x50, 0x75, 0x01, 0x95, 0x04, 0x81,
	0x07, 0x09, 0x06, 0xa1, 0x02, 0x19, 0xb0, 0x29, 0xbb, 0x15, 0x00, 0x25,
	0x0c, 0x75, 0x04, 0x95, 0x01, 0x81, 0x40, 0xc0, 0x09, 0x07, 0x15, 0x00,
	0x25, 0x01, 0x05, 0x09, 0x75, 0x01, 0x95, 0x01, 0x81, 0x02, 0x75, 0x01,
	0x95, 0x05, 0x81, 0x01, 0x05, 0x08, 0x15, 0x00, 0x25, 0x01, 0x09, 0x17,
	0x09, 0x09, 0x09, 0x18, 0x09, 0x20, 0x09, 0x21, 0x75, 0x01, 0x95, 0x05,
	0x91, 0x22, 0x05, 0x0b, 0x15, 0x00, 0x25, 0x01, 0x09, 0x9e, 0x75, 0x01,
	0x95, 0x01, 0x91, 0x22, 0x75, 0x01, 0x95, 0x0a, 0x91, 0x01, 0xc0, 0x06,
	0x00, 0xff, 0x09, 0x01, 0xa1, 0x01, 0x85, 0x03, 0x09, 0x01, 0x15, 0x00,
	0x26, 0xff, 0x00, 0x95, 0x3f, 0x75, 0x08, 0x81, 0x02, 0x09, 0x01, 0x15,
	0x00, 0x26, 0xff, 0x00, 0x95, 0x3f, 0x75, 0x08, 0x91, 0x02, 0xc0,
}

func TestPowerConfLEDsDecodeToReportTwo(t *testing.T) {
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, c := range []struct {
		name  string
		usage uint16
		mask  byte
	}{
		{"off-hook", LEDOffHook, 1 << 0},
		{"mute", LEDMute, 1 << 1},
		{"ring", LEDRing, 1 << 2},
		{"hold", LEDHold, 1 << 3},
		{"mic", LEDMic, 1 << 4},
	} {
		bit, ok := outs.Bit(PageLED, c.usage)
		if !ok {
			t.Fatalf("%s LED missing from parsed outputs", c.name)
		}
		if bit.ReportID != 2 || bit.Byte != 0 || bit.Mask != c.mask {
			t.Errorf("%s LED: got report %d byte %d mask %#02x, want report 2 byte 0 mask %#02x",
				c.name, bit.ReportID, bit.Byte, bit.Mask, c.mask)
		}
	}
	// Input items must not leak into the output map: the hook switch and phone
	// mute keys are buttons the device reports, not controls Voice may drive.
	if _, ok := outs.Bit(PageTelephony, TelHookSwitch); ok {
		t.Error("telephony hook switch input was indexed as an output")
	}
	// Report 2 packs the five LEDs, the telephony ringer bit and ten padding
	// bits, so a valid write carries two payload bytes.
	if got := outs.PayloadBytes(2); got != 2 {
		t.Errorf("report 2 payload = %d bytes, want 2", got)
	}
}

func TestParseOutputsReportsDevicesWithNoOutputs(t *testing.T) {
	// A consumer-control-only device: input items only.
	inputOnly := []byte{
		0x05, 0x0c, 0x09, 0x01, 0xa1, 0x01, 0x85, 0x01, 0x15, 0x00, 0x25, 0x01,
		0x09, 0xe9, 0x75, 0x01, 0x95, 0x01, 0x81, 0x02, 0xc0,
	}
	if _, err := ParseOutputs(inputOnly); err != ErrNoOutputs {
		t.Fatalf("got %v, want ErrNoOutputs", err)
	}
}

func TestParseOutputsRejectsTruncatedDescriptor(t *testing.T) {
	if _, err := ParseOutputs([]byte{0x05}); err == nil {
		t.Fatal("truncated descriptor accepted")
	}
}

// A second writer must never clear the first writer's bits: the off-hook bit
// keeps the PowerConf microphone un-gated for the whole session, so an
// indicator write that dropped it would silence capture.
func TestIndicatorWriteKeepsOffHookBit(t *testing.T) {
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hidraw-fake")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: path, outs: outs, state: map[byte][]byte{}}

	if ok, err := dev.Set(PageLED, LEDOffHook, true); err != nil || !ok {
		t.Fatalf("off-hook: ok=%v err=%v", ok, err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x01, 0x00}) {
		t.Fatalf("off-hook report = % x, want 02 01 00", got)
	}
	if ok, err := dev.Set(PageLED, LEDMic, true); err != nil || !ok {
		t.Fatalf("mic LED: ok=%v err=%v", ok, err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x11, 0x00}) {
		t.Fatalf("mic LED report = % x, want 02 11 00 (off-hook retained)", got)
	}
	if ok, err := dev.Set(PageLED, LEDMic, false); err != nil || !ok {
		t.Fatalf("mic LED off: ok=%v err=%v", ok, err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x01, 0x00}) {
		t.Fatalf("mic LED cleared report = % x, want 02 01 00 (off-hook retained)", got)
	}
}

func TestSetReportsUnadvertisedUsageWithoutWriting(t *testing.T) {
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hidraw-fake")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: path, outs: outs, state: map[byte][]byte{}}
	// Message-waiting (0x19) is not advertised by this device.
	ok, err := dev.Set(PageLED, 0x19, true)
	if ok || err != nil {
		t.Fatalf("got ok=%v err=%v, want false, nil", ok, err)
	}
	if got := read(t, path); len(got) != 0 {
		t.Fatalf("wrote % x for an unadvertised usage", got)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A device whose descriptor cannot be read still has to be taken off hook, and
// firmware treats a short report as a different report: an undersized off-hook
// write leaves the microphone gated. The fallback must therefore carry the
// standard two-byte payload even with no parsed descriptor.
func TestFallbackOffHookWritesFullStandardReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidraw-fake")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: path, state: map[byte][]byte{}} // outs nil: descriptor unreadable
	if dev.Has(PageLED, LEDOffHook) {
		t.Fatal("a device with no parsed descriptor must advertise nothing")
	}
	if err := dev.SetReportBit(StandardOffHook, true); err != nil {
		t.Fatalf("fallback off-hook: %v", err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x01, 0x00}) {
		t.Fatalf("fallback off-hook report = % x, want 02 01 00", got)
	}
	if err := dev.SetReportBit(StandardOffHook, false); err != nil {
		t.Fatalf("fallback clear: %v", err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x00, 0x00}) {
		t.Fatalf("fallback cleared report = % x, want 02 00 00", got)
	}
}

// The capture path and the feedback path write the same report from different
// goroutines. Every write must be a whole, correctly sized report, and the
// off-hook bit capture depends on must survive all of them.
func TestConcurrentWritersNeverEmitATornReport(t *testing.T) {
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hidraw-fake")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: path, outs: outs, state: map[byte][]byte{}}
	if ok, err := dev.Set(PageLED, LEDOffHook, true); !ok || err != nil {
		t.Fatalf("off-hook: ok=%v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	for _, usage := range []uint16{LEDMic, LEDRing, LEDMute, LEDHold} {
		for range 25 {
			wg.Add(1)
			go func(u uint16) {
				defer wg.Done()
				if _, err := dev.Set(PageLED, u, true); err != nil {
					t.Error(err)
				}
				if _, err := dev.Set(PageLED, u, false); err != nil {
					t.Error(err)
				}
			}(usage)
		}
	}
	wg.Wait()

	got := read(t, path)
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("final report = % x, want a three-byte report 2", got)
	}
	if got[1]&0x01 == 0 {
		t.Fatalf("final report = % x: indicator writes cleared the off-hook bit", got)
	}
}

// A successful report write logged nothing, so a light that did not come on
// was indistinguishable from a write that never happened - which is exactly
// what happened on the device: the raw report 02 09 00 lit the ring while
// Voice's own path lit nothing and reported no error.
//
// The bytes and the LENGTH both matter: firmware treats a report of the wrong
// length as a different report, so a log line without the length cannot
// explain a silent failure.
func TestWriteLogsTheExactReportBytesAndLength(t *testing.T) {
	node := filepath.Join(t.TempDir(), "hidraw-test")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tel := &Telephony{path: node, state: map[byte][]byte{}}
	var lines []string
	tel.SetLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if err := tel.SetReportBit(StandardOffHook, true); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("want one log line per write, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{"stage=hid", "bytes=", "len=", "on=true"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line lacks %q: %s", want, lines[0])
		}
	}
	// The retained state must be visible across writes: a second bit written
	// into the same report has to show BOTH bits on the wire, which is the
	// property that would have explained the missing light on the device.
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: node, outs: outs, state: map[byte][]byte{}}
	var wire []string
	dev.SetLogger(func(format string, args ...any) {
		wire = append(wire, fmt.Sprintf(format, args...))
	})
	if err := dev.SetReportBit(StandardOffHook, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := dev.Set(PageLED, LEDHold, true); err != nil || !ok {
		t.Fatalf("hold LED write: ok=%v err=%v", ok, err)
	}
	if len(wire) != 2 {
		t.Fatalf("want two log lines, got %d: %v", len(wire), wire)
	}
	// Off-hook is bit 0 and hold is bit 3, so the second report must carry
	// both: 0x09. A report showing only 0x08 would be the missing-light bug,
	// and before this logging existed nothing could tell the two apart.
	//
	// The assertion reads the bytes FIELD, not the whole line. Matching "09"
	// anywhere in the line passed 6.8% of the time on a deliberately broken
	// payload, because t.TempDir() puts a random number in path= - a check
	// that could pass for the wrong reason, inside the test written to prove
	// the bytes are right.
	got := bytesField(t, wire[1])
	if got != "02 09 00" {
		t.Errorf("second report carried %q, want %q (the retained off-hook bit plus hold)", got, wire[1])
	}
}

// bytesField extracts the bytes= field so an assertion cannot accidentally
// match the random path or the length instead of the payload.
func bytesField(t *testing.T, line string) string {
	t.Helper()
	const key = "bytes="
	i := strings.Index(line, key)
	if i < 0 {
		t.Fatalf("log line has no %s field: %s", key, line)
	}
	rest := line[i+len(key):]
	if j := strings.Index(rest, " len="); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("log line has no len= field after the bytes: %s", line)
	return ""
}

// A successful-write log cannot distinguish "wrote the wrong bytes" from
// "never wrote at all", because the second case leaves no line. The ready
// line is what makes that absence meaningful: present, with no write lines
// after it, is a diagnosis rather than a gap.
func TestLogReadyRecordsThePathAndCapabilities(t *testing.T) {
	outs, err := ParseOutputs(powerConfDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	dev := &Telephony{path: "/dev/hidraw-test", outs: outs, state: map[byte][]byte{}}
	var lines []string
	dev.SetLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	dev.LogReady()
	if len(lines) != 1 {
		t.Fatalf("want one ready line, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{"stage=hid", "state=ready", "/dev/hidraw-test", "LED"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("ready line lacks %q: %s", want, lines[0])
		}
	}

	// Nil-safe: an unconfigured device must not panic, and must not claim to
	// be ready either.
	var none *Telephony
	none.SetLogger(func(string, ...any) { t.Error("nil device logged a ready line") })
	none.LogReady()
}
