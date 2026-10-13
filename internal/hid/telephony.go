package hid

import (
	"errors"
	"fmt"
	"io"
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

	mu    sync.Mutex
	outs  *Outputs
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
	t, err := NewReconnecting(path)
	var warning *DescriptorWarning
	if err != nil && !errors.As(err, &warning) {
		return nil, err
	}
	return t, err
}

// DescriptorWarning means the node exists but its descriptor is unavailable,
// invalid, or unverified. Capture may still use StandardOffHook. Other errors
// from Reopen mean the configured node is unavailable and must reject capture.
type DescriptorWarning struct {
	Err error
}

func (w *DescriptorWarning) Error() string { return w.Err.Error() }
func (w *DescriptorWarning) Unwrap() error { return w.Err }

// NewReconnecting retains a stable shared handle even when the configured node
// is absent. Only an empty path returns a nil handle. Call Reopen before each
// new capture attempt, and do not ignore errors other than DescriptorWarning.
func NewReconnecting(path string) (*Telephony, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("no hidraw path configured")
	}
	t := &Telephony{path: path, state: map[byte][]byte{}}
	return t, t.Reopen()
}

// Reopen resolves the configured path again, refreshes capabilities, and drops
// all retained report bits, including when the node is absent. Call only after
// releasing the previous capture's off-hook bit, before taking the next capture
// off hook. Feedback and capture continue sharing this same handle.
func (t *Telephony) Reopen() error {
	return t.reopen(ReadDescriptor)
}

func (t *Telephony) reopen(readDescriptor func(string) ([]byte, error)) error {
	if t == nil {
		return fmt.Errorf("no telephony HID")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.outs = nil
	clear(t.state)
	if _, err := os.Stat(t.path); err != nil {
		return fmt.Errorf("hidraw %s: %w (install the Voice udev rule)", t.path, err)
	}
	desc, readErr := readDescriptor(t.path)
	if len(desc) == 0 {
		if readErr == nil {
			readErr = errors.New("empty report descriptor")
		}
		return &DescriptorWarning{Err: fmt.Errorf("read report descriptor for %s: %w", t.path, readErr)}
	}
	outs, err := ParseOutputs(desc)
	if err != nil {
		return &DescriptorWarning{Err: fmt.Errorf("parse report descriptor for %s: %w", t.path, err)}
	}
	t.outs = outs
	if readErr != nil {
		return &DescriptorWarning{Err: fmt.Errorf("report descriptor for %s parsed but unverified: %w", t.path, readErr)}
	}
	return nil
}

// hidiocgrdescsize is HIDIOCGRDESCSIZE: _IOR('H', 0x01, int). The kernel is
// the only authority on how long a device's report descriptor is, so Voice
// asks it rather than trusting the number of bytes a read happened to yield.
const hidiocgrdescsize = 0x80044801

// ReadDescriptor reads the HID report descriptor sysfs exports for a hidraw
// node or a stable udev symlink to one, for example /dev/hidraw5 ->
// /sys/class/hidraw/hidraw5/device/report_descriptor, and cross-checks its
// length against the kernel's.
//
// A length mismatch returns no descriptor at all: a short read is a different
// descriptor, and a prefix of a descriptor can parse cleanly while describing
// a device that does not exist. When the length cannot be checked the
// descriptor is returned with an error, so callers can use it and say it is
// unverified.
func ReadDescriptor(devPath string) ([]byte, error) {
	node, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(node)
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
	f, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.describeLocked()
}

func (t *Telephony) describeLocked() string {
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
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.outs.Bit(page, usage)
	return ok
}

// Set drives one advertised usage. Unadvertised usages return false so callers
// can fall back instead of writing bits the firmware never claimed.
func (t *Telephony) Set(page, usage uint16, on bool) (bool, error) {
	if t == nil {
		return false, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	bit, ok := t.outs.Bit(page, usage)
	if !ok {
		return false, nil
	}
	return true, t.writeLocked(bit, on)
}

// SetReportBit drives an explicit report bit, for devices whose descriptor
// could not be parsed but which implement the standard telephony report.
func (t *Telephony) SetReportBit(bit Bit, on bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeLocked(bit, on)
}

// SetLogger installs a callback receiving the exact report bytes written.
// Calls are serialized with report writes; the callback must not call Telephony
// methods. The device seat could
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
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logfLocked("stage=hid state=ready path=%s caps=%s", t.path, t.describeLocked())
}

func (t *Telephony) logfLocked(format string, args ...any) {
	if t.log == nil {
		return
	}
	t.log(format, args...)
}

// writeLocked serialises retained state and physical I/O. Holding t.mu across
// both prevents a stale snapshot from clearing another writer's off-hook bit.
func (t *Telephony) writeLocked(bit Bit, on bool) error {
	payload := t.payload(bit)
	if on {
		payload[bit.Byte] |= bit.Mask
	} else {
		payload[bit.Byte] &^= bit.Mask
	}
	report := make([]byte, 0, len(payload)+1)
	report = append(report, bit.ReportID)
	report = append(report, payload...)

	// Use raw nonblocking I/O: os.File.Write may wait for readiness even when
	// the descriptor was opened with O_NONBLOCK. A disconnected or congested
	// hidraw node must not hold the shared capture/feedback lock indefinitely.
	fd, err := syscall.Open(t.path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w (install the Voice udev rule)", t.path, err)
	}
	n, err := syscall.Write(fd, report)
	if err == nil && n != len(report) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return errors.Join(fmt.Errorf("write report %d to %s: %w", bit.ReportID, t.path, err), syscall.Close(fd))
	}
	// The close is part of the write: a report that fails to reach the device
	// leaves the microphone gated or the light wrong, and a deferred close
	// would hide that.
	if err := syscall.Close(fd); err != nil {
		return fmt.Errorf("close %s after report %d: %w", t.path, bit.ReportID, err)
	}
	// The bytes, the length and the state, because a light that does not come
	// on is otherwise indistinguishable from a light that was never written:
	// firmware treats a report of the wrong length as a different report, so
	// the length is part of the evidence rather than a detail.
	t.logfLocked("stage=hid state=written path=%s report=%d bytes=% x len=%d bit=byte%d/mask%#02x on=%v",
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
