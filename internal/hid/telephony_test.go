package hid

import (
	"os"
	"path/filepath"
	"testing"
)

// powerConfDescriptor is an Anker PowerConf (USB 291a:3301) report descriptor.
// The consumer and telephony input sections are the bytes dumped from the
// installed device at /sys/class/hidraw/hidraw5/device/report_descriptor; the
// LED output section encodes the report the same device reported: report 2,
// Off-Hook/Mute/Ring/Hold/Microphone in bits 0-4.
var powerConfDescriptor = []byte{
	0x05, 0x0c, 0x09, 0x01, 0xa1, 0x01, 0x85, 0x01, 0x15, 0x00, 0x25, 0x01,
	0x09, 0xe9, 0x09, 0xea, 0x09, 0xe2, 0x09, 0xcd, 0x09, 0xb5, 0x09, 0xb6,
	0x09, 0xb3, 0x09, 0xb7, 0x75, 0x01, 0x95, 0x08, 0x81, 0x42, 0xc0,
	0x05, 0x0b, 0x09, 0x05, 0xa1, 0x01, 0x85, 0x02,
	0x05, 0x0b, 0x15, 0x00, 0x25, 0x01, 0x09, 0x20, 0x09, 0x97, 0x75, 0x01,
	0x95, 0x02, 0x81, 0x23,
	0x09, 0x2f, 0x09, 0x21, 0x09, 0x70, 0x09, 0x50, 0x75, 0x01, 0x95, 0x04,
	0x81, 0x07,
	0x05, 0x08, 0x09, 0x17, 0x09, 0x09, 0x09, 0x18, 0x09, 0x20, 0x09, 0x21,
	0x75, 0x01, 0x95, 0x05, 0x91, 0x22,
	0x95, 0x03, 0x91, 0x03,
	0xc0,
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
	if got := outs.PayloadBytes(2); got != 1 {
		t.Errorf("report 2 payload = %d bytes, want 1", got)
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
	if got := read(t, path); string(got) != string([]byte{2, 0x01}) {
		t.Fatalf("off-hook report = % x, want 02 01", got)
	}
	if ok, err := dev.Set(PageLED, LEDMic, true); err != nil || !ok {
		t.Fatalf("mic LED: ok=%v err=%v", ok, err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x11}) {
		t.Fatalf("mic LED report = % x, want 02 11 (off-hook retained)", got)
	}
	if ok, err := dev.Set(PageLED, LEDMic, false); err != nil || !ok {
		t.Fatalf("mic LED off: ok=%v err=%v", ok, err)
	}
	if got := read(t, path); string(got) != string([]byte{2, 0x01}) {
		t.Fatalf("mic LED cleared report = % x, want 02 01 (off-hook retained)", got)
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
