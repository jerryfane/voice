package device

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/herdr-voice/internal/proc"
)

// The kernel CEC adapter has one logical-address allocation. Concurrent
// cec-ctl processes reset each other's allocation and lose replies, so all CEC
// operations in this process share one lock.
var cecMu sync.Mutex

// CEC controls an HDMI display using Linux's cec-ctl utility.
type CEC struct {
	Name, Adapter string
	Address       int
	Timeout       time.Duration
}

func NewCEC(id, adapter string, address int) *CEC {
	return &CEC{Name: id, Adapter: adapter, Address: address, Timeout: 8 * time.Second}
}
func (c *CEC) ID() string   { return c.Name }
func (c *CEC) Kind() string { return "tv" }
func (c *CEC) Describe() string {
	return fmt.Sprintf("HDMI-CEC %s logical address %d", c.Adapter, c.Address)
}
func (c *CEC) Capabilities() []Op { return []Op{OpOn, OpOff, OpVolumeUp, OpVolumeDown, OpMute} }
func (c *CEC) run(ctx context.Context, args ...string) (string, error) {
	cecMu.Lock()
	defer cecMu.Unlock()
	argv := append([]string{"cec-ctl", "-d", c.Adapter, "--playback", "--to", strconv.Itoa(c.Address)}, args...)
	r, e := proc.Run(ctx, argv, nil, c.Timeout)
	return string(r.Stdout) + string(r.Stderr), e
}
func (c *CEC) Read(ctx context.Context) (State, error) {
	s, e := c.run(ctx, "--give-device-power-status")
	if e != nil {
		return nil, e
	}
	re := regexp.MustCompile(`pwr-state:\s*([a-z-]+)`)
	m := re.FindStringSubmatch(strings.ToLower(s))
	if len(m) < 2 {
		return nil, fmt.Errorf("TV did not report power status: %s", strings.TrimSpace(s))
	}
	return State{"power": m[1]}, nil
}
func (c *CEC) Apply(ctx context.Context, cmd Command) (State, error) {
	var args []string
	switch cmd.Op {
	case OpOn:
		args = []string{"--image-view-on"}
	case OpOff:
		args = []string{"--standby"}
	case OpVolumeUp:
		args = []string{"--user-control-pressed=ui-cmd=volume-up", "--user-control-released"}
	case OpVolumeDown:
		args = []string{"--user-control-pressed=ui-cmd=volume-down", "--user-control-released"}
	case OpMute:
		args = []string{"--user-control-pressed=ui-cmd=mute", "--user-control-released"}
	default:
		return nil, fmt.Errorf("CEC TV does not support %q", cmd.Op)
	}
	if _, e := c.run(ctx, args...); e != nil {
		return nil, e
	}
	if cmd.Op == OpOn || cmd.Op == OpOff {
		time.Sleep(800 * time.Millisecond)
		return c.Read(ctx)
	}
	return State{"command": string(cmd.Op), "sent": true}, nil
}
