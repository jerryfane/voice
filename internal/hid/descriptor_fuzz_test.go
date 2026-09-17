package hid

import "testing"

// A report descriptor is firmware input: Voice parses whatever the device
// hands over at startup. A malformed one must fail fast rather than hang or
// panic, so the parser is fuzzed over the seeds below plus testdata/fuzz.
func FuzzParseOutputs(f *testing.F) {
	f.Add(powerConfDescriptor)
	// Item counts large enough to walk for minutes before the bounds landed.
	f.Add([]byte("u\x01\x95\x97\x97\x970(\x91\x9100"))
	// Long item, then a truncated short item.
	f.Add([]byte{0xfe, 0x02, 0x00, 0x05})
	f.Fuzz(func(t *testing.T, b []byte) {
		outs, err := ParseOutputs(b)
		if err != nil {
			return
		}
		for _, u := range outs.Usages() {
			bit, ok := outs.Bit(uint16(u>>16), uint16(u))
			if !ok {
				t.Fatalf("usage %#08x reported but not resolvable", u)
			}
			if size := outs.PayloadBytes(bit.ReportID); bit.Byte >= size {
				t.Fatalf("usage %#08x maps to byte %d of a %d-byte payload", u, bit.Byte, size)
			}
		}
	})
}

func TestParseOutputsRejectsImplausibleItemCounts(t *testing.T) {
	// Report size 1, report count 0x91283097, then an output item: a parser
	// without bounds walks two billion fields here.
	desc := []byte{0x75, 0x01, 0x97, 0x97, 0x30, 0x28, 0x91, 0x91}
	if _, err := ParseOutputs(desc); err == nil {
		t.Fatal("implausible output item accepted")
	}
}
