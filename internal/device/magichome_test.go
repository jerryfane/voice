package device

import (
	"context"
	"io"
	"net"
	"testing"
)

func TestMagicHomeColorPacketAndVerifiedState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// discard: the fake device listener; the test fails on its own if it stops early.
	defer ln.Close()
	packets := make(chan []byte, 2)
	go func() {
		for i := range 2 {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			if i == 0 {
				b := make([]byte, 9)
				io.ReadFull(c, b)
				packets <- b
			} else {
				b := make([]byte, 4)
				io.ReadFull(c, b)
				packets <- b
				// discard: the fake device is the other end of the socket; if
				// it cannot write, the client under test fails on its own
				// timeout, which is the assertion that matters.
				c.Write([]byte{0x81, 0x35, 0x23, 0x61, 0x30, 0x1f, 0xff, 0, 0, 0, 0x0a, 0, 0x0f, 0})
			}
			// discard: closing the fake device's connection.
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	m := NewMagicHome("lamp", "127.0.0.1", port, "9byte")
	s, err := m.Apply(context.Background(), Command{Op: OpColor, Args: map[string]any{"name": "red"}})
	if err != nil {
		t.Fatal(err)
	}
	if s["r"] != 255 || s["power"] != "on" {
		t.Fatalf("state=%v", s)
	}
	set := <-packets
	query := <-packets
	want := []byte{0x31, 0xff, 0, 0, 0, 0, 0xf0, 0x0f, 0x2f}
	if string(set) != string(want) {
		t.Fatalf("set=%x want=%x", set, want)
	}
	if len(query) != 4 || query[3] != checksum(query[:3]) {
		t.Fatalf("query=%x", query)
	}
}
