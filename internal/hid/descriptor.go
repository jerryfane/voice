// Package hid talks to USB HID speakerphones through hidraw. Voice needs two
// things from them: the telephony off-hook report that un-gates the microphone,
// and the LED reports that show whether it is listening. Both live in the same
// output reports, so a single owner has to hold the whole report state.
package hid

import (
	"errors"
	"fmt"
)

// HID usage pages and usages Voice cares about.
const (
	PageLED       uint16 = 0x08
	PageTelephony uint16 = 0x0b

	// LED page indicators found on telephony headsets.
	LEDMute    uint16 = 0x09
	LEDOffHook uint16 = 0x17
	LEDRing    uint16 = 0x18
	LEDHold    uint16 = 0x20
	LEDMic     uint16 = 0x21

	// Telephony page controls some firmwares expose as outputs.
	TelHookSwitch uint16 = 0x20
	TelPhoneMute  uint16 = 0x2f
)

// ErrNoOutputs means the device advertises no single-bit output controls, so it
// can neither be taken off hook nor show an indicator.
var ErrNoOutputs = errors.New("device advertises no single-bit HID output controls")

// Bit locates one boolean output inside a report. Payload carries the report's
// full payload length so every writer emits a report of the length the device
// declared: a short write is a different report as far as firmware is
// concerned, which is how an undersized off-hook report can leave a
// speakerphone microphone gated.
type Bit struct {
	ReportID byte
	Byte     int  // index into the report payload, excluding the report ID
	Mask     byte // bit within that byte
	Payload  int  // report payload length in bytes, excluding the report ID
}

// StandardOffHook is the telephony off-hook output that USB speakerphones
// implement at a fixed location: report 2, first payload bit, two payload
// bytes. Voice uses it only when a device's own descriptor is unreadable, so
// capture still un-gates the microphone on hosts where sysfs is unavailable.
var StandardOffHook = Bit{ReportID: 2, Byte: 0, Mask: 1, Payload: 2}

// Outputs is the set of single-bit output controls a device advertises,
// discovered by parsing its report descriptor rather than assuming a layout.
type Outputs struct {
	bits  map[uint32]Bit
	sizes map[byte]int // report ID -> payload bytes, excluding the report ID
}

// Key packs a usage page and usage into the identifier used by Outputs.
func Key(page, usage uint16) uint32 { return uint32(page)<<16 | uint32(usage) }

// Bit reports where a usage lives, if the device advertises it as an output.
func (o *Outputs) Bit(page, usage uint16) (Bit, bool) {
	if o == nil {
		return Bit{}, false
	}
	b, ok := o.bits[Key(page, usage)]
	return b, ok
}

// PayloadBytes is the size of a report's payload, excluding the report ID.
func (o *Outputs) PayloadBytes(id byte) int {
	if o == nil {
		return 0
	}
	return o.sizes[id]
}

// Usages lists the advertised (page, usage) keys, for diagnostics.
func (o *Outputs) Usages() []uint32 {
	if o == nil {
		return nil
	}
	out := make([]uint32, 0, len(o.bits))
	for k := range o.bits {
		out = append(out, k)
	}
	return out
}

// Item tags the parser understands. Everything else only needs its length.
const (
	tagInput         = 0x80
	tagOutput        = 0x90
	tagFeature       = 0xb0
	tagCollection    = 0xa0
	tagEndCollection = 0xc0
	tagUsagePage     = 0x04
	tagReportSize    = 0x74
	tagReportID      = 0x84
	tagReportCount   = 0x94
	tagUsage         = 0x08
	tagUsageMin      = 0x18
	tagUsageMax      = 0x28
	tagPush          = 0xa4
	tagPop           = 0xb4
)

// A descriptor is firmware input, so the parser's work must be bounded by the
// bytes it was handed rather than by numbers those bytes claim. Two bounds do
// that. A report's bit budget is capped by what the hidraw transport can
// carry. And the number of single-bit outputs indexed is capped at eight per
// descriptor byte: a usage range declares a whole span from six bytes, so
// counting declared usages alone is not a length bound. Eight per byte leaves
// real devices a wide margin - the installed speakerphone names six outputs in
// 191 bytes - while keeping the work proportional to the input.
const (
	maxReportBytes     = 4096 // hidraw rejects larger reports than this
	indexedBitsPerByte = 8    // single-bit outputs indexed per descriptor byte
)

// ParseOutputs walks a HID report descriptor and records every one-bit output
// control with its report ID and bit position. Only single-bit outputs are
// indexed: those are the LED and hook controls, and indexing wider fields would
// invite writing values Voice does not understand.
func ParseOutputs(desc []byte) (*Outputs, error) {
	if len(desc) == 0 {
		return nil, errors.New("empty report descriptor")
	}
	o := &Outputs{bits: map[uint32]Bit{}, sizes: map[byte]int{}}
	// HID Push/Pop snapshot the whole global item state, not just the usage
	// page, so the parser keeps them together.
	type globals struct {
		page        uint16
		reportSize  uint32
		reportCount uint32
		reportID    byte
	}
	var (
		g         globals
		usages    []uint32 // packed page|usage, in declaration order
		rangeMin  uint32
		rangeMax  uint32
		haveRange bool
		offsets   = map[byte]uint64{} // report ID -> next free output bit
		pushed    []globals
		depth     int // open collections; a truncated dump never closes them
		indexed   int // single-bit outputs indexed so far
	)
	clearLocals := func() { usages, haveRange = nil, false }
	for i := 0; i < len(desc); {
		b := desc[i]
		if b == 0xfe { // long item: Voice needs none of them
			if i+2 >= len(desc) {
				return nil, fmt.Errorf("truncated long item at byte %d", i)
			}
			i += 3 + int(desc[i+1])
			continue
		}
		size := int(b & 0x03)
		if size == 3 {
			size = 4
		}
		if i+1+size > len(desc) {
			return nil, fmt.Errorf("truncated item at byte %d", i)
		}
		var v uint32
		for s := range size {
			v |= uint32(desc[i+1+s]) << (8 * s)
		}
		switch b & 0xfc {
		case tagUsagePage:
			g.page = uint16(v)
		case tagReportSize:
			g.reportSize = v
		case tagReportCount:
			g.reportCount = v
		case tagReportID:
			g.reportID = byte(v)
		case tagUsage:
			usages = append(usages, packUsage(g.page, v, size))
		case tagUsageMin:
			rangeMin, haveRange = packUsage(g.page, v, size), true
		case tagUsageMax:
			rangeMax, haveRange = packUsage(g.page, v, size), true
		case tagOutput:
			// Check the report's bit budget before walking anything, so a
			// declared count can never drive work the transport could not
			// carry in the first place.
			start := offsets[g.reportID]
			end := start + uint64(g.reportSize)*uint64(g.reportCount)
			bytes := (end + 7) / 8
			if bytes > maxReportBytes {
				return nil, fmt.Errorf("output report %d declares %d bytes, more than the %d hidraw carries", g.reportID, bytes, maxReportBytes)
			}
			offsets[g.reportID] = end
			if int(bytes) > o.sizes[g.reportID] {
				o.sizes[g.reportID] = int(bytes)
			}
			if g.reportSize == 1 {
				fields := named(usages, rangeMin, rangeMax, haveRange, g.reportCount)
				indexed += fields
				if indexed > len(desc)*indexedBitsPerByte {
					return nil, fmt.Errorf("%d-byte report descriptor names %d single-bit outputs, more than %d per byte",
						len(desc), indexed, indexedBitsPerByte)
				}
				for n := range fields {
					u, ok := usageAt(usages, rangeMin, rangeMax, haveRange, n)
					if !ok {
						continue // constant padding: occupies bits, names nothing
					}
					bit := start + uint64(n)
					o.bits[u] = Bit{ReportID: g.reportID, Byte: int(bit / 8), Mask: 1 << (bit % 8)}
				}
			}
			clearLocals()
		case tagCollection:
			depth++
			clearLocals()
		case tagEndCollection:
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced end-collection at byte %d", i)
			}
			clearLocals()
		case tagInput, tagFeature:
			// Input and feature bits live in their own report space, so only
			// the local usage state has to be reset.
			clearLocals()
		case tagPush:
			pushed = append(pushed, g)
		case tagPop:
			if n := len(pushed); n > 0 {
				g, pushed = pushed[n-1], pushed[:n-1]
			}
		}
		i += 1 + size
	}
	if depth != 0 {
		return nil, fmt.Errorf("truncated report descriptor: %d collection(s) never closed in %d bytes", depth, len(desc))
	}
	// A report's payload length is only final once every item for that report
	// has been seen, so stamp it onto each bit at the end: writers must never
	// have to ask a second object how long the report is.
	for u, bit := range o.bits {
		bit.Payload = o.sizes[bit.ReportID]
		o.bits[u] = bit
	}
	if len(o.bits) == 0 {
		return o, ErrNoOutputs
	}
	return o, nil
}

// named reports how many fields of a main item the descriptor actually names.
// It is the floor of the declared count and the declared usages, so a hostile
// or corrupt count cannot drive iteration: the usage list and the usage range
// both come from bytes the caller supplied.
func named(usages []uint32, min, max uint32, haveRange bool, count uint32) int {
	n := len(usages)
	if haveRange && max >= min {
		if span := int(max-min) + 1; span > n {
			n = span
		}
	}
	if int64(count) < int64(n) {
		n = int(count)
	}
	return n
}

// packUsage applies the current usage page, unless the item is a 4-byte
// extended usage that carries its own page in the high half.
func packUsage(page uint16, v uint32, size int) uint32 {
	if size == 4 {
		return v
	}
	return Key(page, uint16(v))
}

func usageAt(usages []uint32, min, max uint32, haveRange bool, n int) (uint32, bool) {
	if n < len(usages) {
		return usages[n], true
	}
	if haveRange && max >= min && uint32(n) <= max-min {
		return min + uint32(n), true
	}
	return 0, false
}
