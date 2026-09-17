package hid

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Telephony owns one hidraw node. Output reports pack several controls into the
// same bytes, so every writer has to go through one object that remembers the
// whole report: writing the off-hook report from the capture path and an LED
// report from the feedback path independently would clear each other's bits and
// silence the microphone.
type Telephony struct {
	path string
	outs *Outputs

	mu    sync.Mutex
	state map[byte][]byte // report ID -> payload, excluding the report ID
}

// Open resolves the device's report descriptor and returns a handle. The node
// itself is opened per write: these reports are rare, and holding a writable
// descriptor open for the life of the service is not worth it.
//
// A descriptor that cannot be read is not fatal: fallback reports the usages
// Voice has always assumed for USB telephony devices, so capture keeps working
// on hosts where sysfs is not reachable.
func Open(path string) (*Telephony, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("no hidraw path configured")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("hidraw %s: %w (install the Voice udev rule)", path, err)
	}
	t := &Telephony{path: path, state: map[byte][]byte{}}
	desc, err := ReadDescriptor(path)
	if err != nil {
		return t, fmt.Errorf("read report descriptor for %s: %w", path, err)
	}
	outs, err := ParseOutputs(desc)
	t.outs = outs
	if err != nil {
		return t, fmt.Errorf("parse report descriptor for %s: %w", path, err)
	}
	return t, nil
}

// ReadDescriptor reads the HID report descriptor that sysfs exports for a
// hidraw node, for example /dev/hidraw5 ->
// /sys/class/hidraw/hidraw5/device/report_descriptor.
func ReadDescriptor(devPath string) ([]byte, error) {
	name := filepath.Base(devPath)
	if !strings.HasPrefix(name, "hidraw") {
		return nil, fmt.Errorf("%s is not a hidraw node", devPath)
	}
	return os.ReadFile(filepath.Join("/sys/class/hidraw", name, "device", "report_descriptor"))
}

// Describe names the device and the controls it advertises.
func (t *Telephony) Describe() string {
	if t == nil {
		return "no telephony HID"
	}
	var have []string
	for _, c := range []struct {
		name  string
		page  uint16
		usage uint16
	}{
		{"off-hook LED", PageLED, LEDOffHook},
		{"mute LED", PageLED, LEDMute},
		{"ring LED", PageLED, LEDRing},
		{"mic LED", PageLED, LEDMic},
		{"hook switch", PageTelephony, TelHookSwitch},
		{"phone mute", PageTelephony, TelPhoneMute},
	} {
		if _, ok := t.outs.Bit(c.page, c.usage); ok {
			have = append(have, c.name)
		}
	}
	if len(have) == 0 {
		return t.path + " (no known output controls)"
	}
	return t.path + " (" + strings.Join(have, ", ") + ")"
}

// Has reports whether the device advertises a usage as a writable output.
func (t *Telephony) Has(page, usage uint16) bool {
	if t == nil {
		return false
	}
	_, ok := t.outs.Bit(page, usage)
	return ok
}

// Set drives one advertised usage. Unadvertised usages return false so callers
// can fall back instead of writing bits the firmware never claimed.
func (t *Telephony) Set(page, usage uint16, on bool) (bool, error) {
	if t == nil {
		return false, nil
	}
	bit, ok := t.outs.Bit(page, usage)
	if !ok {
		return false, nil
	}
	return true, t.write(bit, on)
}

// SetReportBit drives an explicit report bit, for devices whose descriptor
// could not be parsed but which implement the standard telephony report.
func (t *Telephony) SetReportBit(bit Bit, on bool) error {
	if t == nil {
		return nil
	}
	return t.write(bit, on)
}

func (t *Telephony) write(bit Bit, on bool) error {
	t.mu.Lock()
	payload := t.payload(bit)
	if on {
		payload[bit.Byte] |= bit.Mask
	} else {
		payload[bit.Byte] &^= bit.Mask
	}
	report := make([]byte, 0, len(payload)+1)
	report = append(report, bit.ReportID)
	report = append(report, payload...)
	t.mu.Unlock()

	f, err := os.OpenFile(t.path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w (install the Voice udev rule)", t.path, err)
	}
	defer f.Close()
	if _, err := f.Write(report); err != nil {
		return fmt.Errorf("write report %d to %s: %w", bit.ReportID, t.path, err)
	}
	return nil
}

// payload returns the retained bytes for a report, sized from the descriptor
// when known and from the touched bit otherwise. Callers hold t.mu.
func (t *Telephony) payload(bit Bit) []byte {
	p := t.state[bit.ReportID]
	want := t.outs.PayloadBytes(bit.ReportID)
	if want <= bit.Byte {
		want = bit.Byte + 1
	}
	if len(p) < want {
		grown := make([]byte, want)
		copy(grown, p)
		p = grown
		t.state[bit.ReportID] = p
	}
	return p
}
