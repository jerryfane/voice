package device

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// MagicHome controls Zengge/LEDENET RGB(W) controllers directly over TCP 5577.
type MagicHome struct {
	Name, Host string
	Port       int
	Protocol   string
	Timeout    time.Duration
}

func NewMagicHome(id, host string, port int, protocol string) *MagicHome {
	if port == 0 {
		port = 5577
	}
	if protocol == "" {
		protocol = "auto"
	}
	return &MagicHome{Name: id, Host: host, Port: port, Protocol: protocol, Timeout: 3 * time.Second}
}
func (m *MagicHome) ID() string   { return m.Name }
func (m *MagicHome) Kind() string { return "light" }
func (m *MagicHome) Describe() string {
	return fmt.Sprintf("Magic Home %s:%d (%s)", m.Host, m.Port, m.Protocol)
}
func (m *MagicHome) Capabilities() []Op {
	return []Op{OpOn, OpOff, OpToggle, OpColor, OpBrightness, OpWhite}
}
func checksum(b []byte) byte {
	var n byte
	for _, v := range b {
		n += v
	}
	return n
}
func (m *MagicHome) roundTrip(ctx context.Context, p []byte, n int) ([]byte, error) {
	d := net.Dialer{Timeout: m.Timeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(m.Host, strconv.Itoa(m.Port)))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(m.Timeout)); err != nil {
		return nil, err
	}
	if _, err = c.Write(p); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	b := make([]byte, n)
	got := 0
	for got < n {
		x, e := c.Read(b[got:])
		got += x
		if e != nil {
			return nil, e
		}
	}
	return b, nil
}
func (m *MagicHome) Read(ctx context.Context) (State, error) {
	q := []byte{0x81, 0x8a, 0x8b}
	q = append(q, checksum(q))
	r, err := m.roundTrip(ctx, q, 14)
	if err != nil {
		return nil, err
	}
	if len(r) < 14 {
		return nil, fmt.Errorf("short state response: %s", hex.EncodeToString(r))
	}
	power := "off"
	if r[2] == 0x23 {
		power = "on"
	}
	return State{"power": power, "mode": fmt.Sprintf("0x%02x", r[3]), "r": int(r[6]), "g": int(r[7]), "b": int(r[8]), "white": int(r[9]), "device_type": fmt.Sprintf("0x%02x", r[1])}, nil
}
func (m *MagicHome) Apply(ctx context.Context, c Command) (State, error) {
	switch c.Op {
	case OpOn, OpOff:
		v := byte(0x23)
		if c.Op == OpOff {
			v = 0x24
		}
		p := []byte{0x71, v, 0x0f}
		p = append(p, checksum(p))
		if _, err := m.roundTrip(ctx, p, 0); err != nil {
			return nil, err
		}
	case OpToggle:
		s, e := m.Read(ctx)
		if e != nil {
			return nil, e
		}
		op := OpOn
		if s["power"] == "on" {
			op = OpOff
		}
		return m.Apply(ctx, Command{Op: op})
	case OpColor:
		r, g, b, err := colorArgs(c.Args)
		if err != nil {
			return nil, err
		}
		if err = m.set(ctx, r, g, b, 0, 0, 0xf0); err != nil {
			return nil, err
		}
	case OpWhite, OpBrightness:
		level, err := number(c.Args, "level", 100)
		if err != nil {
			return nil, err
		}
		if level < 0 || level > 100 {
			return nil, fmt.Errorf("level must be 0-100")
		}
		w := byte(level * 255 / 100)
		if err = m.set(ctx, 0, 0, 0, w, 0, 0x0f); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Magic Home does not support %q", c.Op)
	}
	time.Sleep(120 * time.Millisecond)
	return m.Read(ctx)
}
func (m *MagicHome) set(ctx context.Context, r, g, b, w1, w2, mask byte) error {
	body := []byte{0x31, r, g, b, w1, w2, mask, 0x0f}
	body = append(body, checksum(body))
	_, err := m.roundTrip(ctx, body, 0)
	return err
}
func number(a map[string]any, k string, def int) (int, error) {
	if a == nil {
		return def, nil
	}
	v, ok := a[k]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case string:
		x, e := strconv.Atoi(n)
		return x, e
	default:
		return 0, fmt.Errorf("%s must be a number", k)
	}
}
func colorArgs(a map[string]any) (byte, byte, byte, error) {
	if name, ok := a["name"].(string); ok {
		if c, yes := colors[strings.ToLower(name)]; yes {
			return c[0], c[1], c[2], nil
		}
		return 0, 0, 0, fmt.Errorf("unknown color %q", name)
	}
	r, e := number(a, "r", 0)
	if e != nil {
		return 0, 0, 0, e
	}
	g, e := number(a, "g", 0)
	if e != nil {
		return 0, 0, 0, e
	}
	b, e := number(a, "b", 0)
	if e != nil {
		return 0, 0, 0, e
	}
	for _, v := range []int{r, g, b} {
		if v < 0 || v > 255 {
			return 0, 0, 0, fmt.Errorf("RGB values must be 0-255")
		}
	}
	return byte(r), byte(g), byte(b), nil
}

var colors = map[string][3]byte{"red": {255, 0, 0}, "green": {0, 255, 0}, "blue": {0, 0, 255}, "purple": {160, 0, 255}, "orange": {255, 80, 0}, "yellow": {255, 200, 0}, "cyan": {0, 255, 255}, "pink": {255, 30, 120}, "white": {255, 255, 255}}
