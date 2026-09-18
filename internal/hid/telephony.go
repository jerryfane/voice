package hid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
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
	log   func(format string, args ...any)
}

// Open resolves the device's report descriptor and returns a handle. The node
// itself is opened per write: these reports are rare, and holding a writable
// descriptor open for the life of the service is not worth it.
//
// A descriptor that cannot be read, cannot be trusted, or does not parse is
// not fatal: the handle falls back to StandardOffHook so capture still
// un-gates the microphone, and it advertises no indicator. It never retains a
// partially parsed map, because half a descriptor maps bits with confidence it
// has not earned.
func Open(path string) (*Telephony, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("no hidraw path configured")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("hidraw %s: %w (install the Voice udev rule)", path, err)
	}
	t := &Telephony{path: path, state: map[byte][]byte{}}
	desc, readErr := ReadDescriptor(path)
	if len(desc) == 0 {
		return t, fmt.Errorf("read report descriptor for %s: %w", path, readErr)
	}
	outs, err := ParseOutputs(desc)
	if err != nil {
		return t, fmt.Errorf("parse report descriptor for %s: %w", path, err)
	}
	t.outs = outs
	if readErr != nil {
		return t, fmt.Errorf("report descriptor for %s parsed but unverified: %w", path, readErr)
	}
	return t, nil
}

// hidiocgrdescsize is HIDIOCGRDESCSIZE: _IOR('H', 0x01, int). The kernel is
// the only authority on how long a device's report descriptor is, so Voice
// asks it rather than trusting the number of bytes a read happened to yield.
const hidiocgrdescsize = 0x80044801

// ReadDescriptor reads the HID report descriptor sysfs exports for a hidraw
// node, for example /dev/hidraw5 ->
// /sys/class/hidraw/hidraw5/device/report_descriptor, and cross-checks its
// length against the kernel's.
//
// A length mismatch returns no descriptor at all: a short read is a different
// descriptor, and a prefix of a descriptor can parse cleanly while describing
// a device that does not exist. When the length cannot be checked the
// descriptor is returned with an error, so callers can use it and say it is
// unverified.
func ReadDescriptor(devPath string) ([]byte, error) {
	name := filepath.Base(devPath)
	if !strings.HasPrefix(name, "hidraw") {
		return nil, fmt.Errorf("%s is not a hidraw node", devPath)
	}
	desc, err := os.ReadFile(filepath.Join("/sys/class/hidraw", name, "device", "report_descriptor"))
	if err != nil {
		return nil, err
	}
	size, err := descriptorSize(devPath)
	if err != nil {
		return desc, fmt.Errorf("cannot confirm descriptor length, read %d bytes: %w", len(desc), err)
	}
	if size != len(desc) {
		return nil, fmt.Errorf("short descriptor read: kernel reports %d bytes, read %d", size, len(desc))
	}
	return desc, nil
}

func descriptorSize(devPath string) (size int, err error) {
	f, err := os.Open(devPath)
	if err != nil {
		return 0, err
	}
	// Nothing is written here, so a close failure cannot lose data - but it
	// does mean the handle Voice thought it released is still open, which is
	// worth carrying out with the answer.
	defer func() { err = errors.Join(err, f.Close()) }()
	var n int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), hidiocgrdescsize, uintptr(unsafe.Pointer(&n))); errno != 0 {
		return 0, fmt.Errorf("HIDIOCGRDESCSIZE on %s: %w", devPath, errno)
	}
	return int(n), nil
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

// write serialises both the retained report state and the physical write. The
// lock is held across the I/O on purpose: the capture path and the feedback
// path write the same report, and releasing the lock before the write would
// let a stale snapshot land on the device after a newer one.
// Log, if set, receives the exact report bytes written. The device seat could
// not tell whether a light that did not come on was a write that never
// happened, a write of the wrong bytes, or firmware ignoring a correct
// report - because a successful write logged nothing at all. Bytes on the
// wire are the only evidence that separates those.
func (t *Telephony) SetLogger(f func(format string, args ...any)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.log = f
}

// LogReady records that the device is open and what it can drive, so that the
// ABSENCE of later write lines is itself a diagnosis. A successful-write log
// cannot distinguish "wrote the wrong bytes" from "never wrote at all",
// because the second case leaves no line at all; a ready line with nothing
// after it answers that on its own.
func (t *Telephony) LogReady() {
	if t == nil {
		return
	}
	t.logf("stage=hid state=ready path=%s caps=%s", t.path, t.Describe())
}

func (t *Telephony) logf(format string, args ...any) {
	if t == nil || t.log == nil {
		return
	}
	t.log(format, args...)
}

func (t *Telephony) write(bit Bit, on bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	payload := t.payload(bit)
	if on {
		payload[bit.Byte] |= bit.Mask
	} else {
		payload[bit.Byte] &^= bit.Mask
	}
	report := make([]byte, 0, len(payload)+1)
	report = append(report, bit.ReportID)
	report = append(report, payload...)

	f, err := os.OpenFile(t.path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w (install the Voice udev rule)", t.path, err)
	}
	if _, err := f.Write(report); err != nil {
		return errors.Join(fmt.Errorf("write report %d to %s: %w", bit.ReportID, t.path, err), f.Close())
	}
	// The close is part of the write: a report that fails to reach the device
	// leaves the microphone gated or the light wrong, and a deferred close
	// would hide that.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s after report %d: %w", t.path, bit.ReportID, err)
	}
	// The bytes, the length and the state, because a light that does not come
	// on is otherwise indistinguishable from a light that was never written:
	// firmware treats a report of the wrong length as a different report, so
	// the length is part of the evidence rather than a detail.
	t.logf("stage=hid state=written path=%s report=%d bytes=% x len=%d bit=byte%d/mask%#02x on=%v",
		t.path, bit.ReportID, report, len(report), bit.Byte, bit.Mask, on)
	return nil
}

// payload returns the retained bytes for a report, sized from the length the
// Bit carries. Firmware treats a short report as a different report, so the
// length must not depend on whether the descriptor happened to parse: a
// descriptor-derived Bit and StandardOffHook both state their own length.
// Callers hold t.mu.
func (t *Telephony) payload(bit Bit) []byte {
	p := t.state[bit.ReportID]
	want := bit.Payload
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
