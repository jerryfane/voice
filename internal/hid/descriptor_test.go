package hid

import "testing"

// HID Push/Pop snapshot the whole global item state, not just the usage page.
// A descriptor that pushes, changes report id/size/count inside a nested
// collection and pops must resolve its later outputs against the restored
// state, or the parser silently maps bits to the wrong report and offset.
func TestPushPopRestoresEveryGlobalItem(t *testing.T) {
	desc := []byte{
		0x05, 0x08, // usage page LED
		0x85, 0x02, // report ID 2
		0x75, 0x01, // report size 1
		0x95, 0x01, // report count 1
		0xa4, // push: page LED, report 2, size 1, count 1

		0x05, 0x0b, // usage page telephony
		0x85, 0x07, // report ID 7
		0x75, 0x08, // report size 8
		0x95, 0x04, // report count 4
		0x09, 0x20, // usage hook switch
		0x91, 0x02, // output: four bytes in report 7

		0xb4, // pop: back to page LED, report 2, size 1, count 1

		0x09, 0x17, // usage LED off-hook
		0x91, 0x02, // output: one bit in report 2
	}
	outs, err := ParseOutputs(desc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	bit, ok := outs.Bit(PageLED, LEDOffHook)
	if !ok {
		t.Fatal("off-hook LED not resolved after a pop")
	}
	if bit.ReportID != 2 || bit.Byte != 0 || bit.Mask != 0x01 || bit.Payload != 1 {
		t.Errorf("off-hook after pop = report %d byte %d mask %#02x payload %d, want report 2 byte 0 mask 0x01 payload 1",
			bit.ReportID, bit.Byte, bit.Mask, bit.Payload)
	}
	if got := outs.PayloadBytes(7); got != 4 {
		t.Errorf("report 7 payload = %d, want 4 (the pushed state's own item)", got)
	}
}

// A dump cut short at an item boundary parses as valid items but describes an
// unterminated device. The first PowerConf dump was truncated exactly this
// way, and it must be rejected rather than silently yielding a partial map.
func TestTruncatedDumpIsRejectedNotPartiallyParsed(t *testing.T) {
	// 92 bytes ends on an item boundary inside the telephony collection, so
	// every item parses and only the unterminated collection reveals the cut.
	// The device's first dump reached the same region.
	for _, n := range []int{81, 92, 100} {
		outs, err := ParseOutputs(powerConfDescriptor[:n])
		if err == nil {
			t.Errorf("%d-byte dump accepted, mapping %d usages", n, len(outs.Usages()))
		}
	}
}
