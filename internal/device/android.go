package device

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/proc"
)

const androidTimeout = 10 * time.Second

// Android controls one already authorized ADB target. Legacy structured commands
// restrict values reaching the remote shell; opt-in agent control intentionally
// exposes the phone shell through tool-only ADB and Observe methods.
// Activity-manager acknowledgements and keyevent exits are request receipts,
// not proof of foreground UI, audio playback, volume, or a completed call.
type Android struct {
	id, serial   string
	ops          []Op
	apps         map[string]string
	err          error
	agentControl bool
}

// NewAndroid does no I/O. Invalid config grants no capabilities and fails both
// Read and Apply, including when built without first validating the deployment.
func NewAndroid(cfg config.Android) *Android {
	d := &Android{id: cfg.ID, serial: cfg.Serial, err: cfg.Validate()}
	if d.err != nil {
		return d
	}
	d.agentControl = cfg.AgentControl
	d.ops = make([]Op, len(cfg.Operations))
	for i, op := range cfg.Operations {
		d.ops[i] = Op(op)
	}
	d.apps = make(map[string]string, len(cfg.Apps))
	for alias, pkg := range cfg.Apps {
		d.apps[alias] = pkg
	}
	return d
}

func (d *Android) ID() string   { return d.id }
func (d *Android) Kind() string { return "phone" }
func (d *Android) Capabilities() []Op {
	return append([]Op(nil), d.ops...)
}

func (d *Android) allows(op Op) bool {
	for _, allowed := range d.ops {
		if allowed == op {
			return true
		}
	}
	return false
}

func (d *Android) Describe() string {
	if d.agentControl {
		return "Authorized Android phone with general tool-based control, including apps and private screen/data access. Use `voice phone " + d.id + " observe` for JSON UI hierarchy and a private local PNG path; read the PNG visually. Use `voice phone " + d.id + " adb <ADB command and args...>` for arbitrary phone commands, including shell input, am, content, and app workflows. The configured serial is pinned; host transport overrides are denied. No per-app or per-action allowlist applies to this tool path. Observe, act, then observe to verify the requested outcome; a successful command exit is not proof of UI outcome. Keep private output in tools, not generic spoken receipts. Legacy structured operations remain available only as configured."
	}
	aliases := make([]string, 0, len(d.apps))
	if d.allows(OpOpenApp) {
		for alias := range d.apps {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	apps := "none"
	if len(aliases) > 0 {
		apps = strings.Join(aliases, ", ")
	}
	return "Authorized Android phone; open_app requires app alias (allowed: " + apps + "); dial requires number (1-15 digits, optional leading +) and only opens the dialer for confirmation on the phone; read checks connection only; volume_up, volume_down, pause, resume take no args; requests do not verify foreground or playback; no lock bypass or private data access"
}

func (d *Android) Read(ctx context.Context) (State, error) {
	return d.Apply(ctx, Command{Op: OpRead})
}

func (d *Android) Apply(ctx context.Context, cmd Command) (State, error) {
	if d.err != nil {
		return nil, d.err
	}
	if !d.allows(cmd.Op) {
		return nil, errors.New("Android operation is not allowed")
	}
	args, receipt, err := d.command(cmd)
	if err != nil {
		return nil, err
	}
	// One deadline covers both connection check and mutation, not ten seconds
	// per subprocess. A shorter caller deadline or cancellation wins.
	ctx, cancel := context.WithTimeout(ctx, androidTimeout)
	defer cancel()
	out, err := d.run(ctx, "get-state")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(out.Stdout)) != "device" {
		return nil, errors.New("Android target is not connected and authorized; check the phone and ADB trust")
	}
	if cmd.Op == OpRead {
		return State{"state": "connected"}, nil
	}
	out, err = d.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if cmd.Op == OpOpenApp || cmd.Op == OpDial {
		if !androidActivityAccepted(out) {
			return nil, errors.New("Android did not acknowledge the activity request; check the phone")
		}
	}
	return State{"receipt": receipt}, nil
}

func (d *Android) run(ctx context.Context, args ...string) (proc.Result, error) {
	if err := ctx.Err(); err != nil {
		return proc.Result{}, err
	}
	argv := make([]string, 0, 3+len(args))
	argv = append(argv, "adb", "-s", d.serial)
	argv = append(argv, args...)
	out, err := proc.Run(ctx, argv, nil, 0)
	if ctx.Err() != nil {
		return proc.Result{}, ctx.Err()
	}
	if err != nil {
		// proc.Run errors include stderr. Never propagate phone output, dialed
		// numbers, or target identifiers into logs or spoken responses.
		return proc.Result{}, errors.New("Android ADB request failed; check adb installation, connection, and phone authorization")
	}
	return out, nil
}

func (d *Android) command(cmd Command) ([]string, string, error) {
	switch cmd.Op {
	case OpOpenApp:
		alias, ok := cmd.Args["app"].(string)
		pkg, allowed := d.apps[alias]
		if len(cmd.Args) != 1 || !ok || !allowed {
			return nil, "", errors.New("Android open_app requires only an owner-allowed app alias")
		}
		return []string{"shell", "am", "start", "-W", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", pkg},
			"Sent an app launch request to the phone; foreground app not verified.", nil
	case OpDial:
		number, ok := cmd.Args["number"].(string)
		if len(cmd.Args) != 1 || !ok || !androidDialNumber(number) {
			return nil, "", errors.New("Android dial requires only a number string of 1-15 digits with an optional leading plus")
		}
		return []string{"shell", "am", "start", "-W", "-a", "android.intent.action.DIAL", "-d", "tel:" + number},
			"Sent a request to open the phone dialer; confirm the call on the phone. No call was placed by Voice.", nil
	case OpRead, OpVolumeUp, OpVolumeDown, OpPause, OpResume:
		if len(cmd.Args) != 0 {
			return nil, "", errors.New("Android operation accepts no arguments")
		}
		var code, receipt string
		switch cmd.Op {
		case OpRead:
			return nil, "", nil
		case OpVolumeUp:
			code, receipt = "24", "Sent a volume-up request to the phone; volume not verified."
		case OpVolumeDown:
			code, receipt = "25", "Sent a volume-down request to the phone; volume not verified."
		case OpPause:
			code, receipt = "127", "Sent a media-pause request to the phone; playback not verified."
		case OpResume:
			code, receipt = "126", "Sent a media-resume request to the phone; playback not verified."
		}
		return []string{"shell", "input", "keyevent", code}, receipt, nil
	default:
		return nil, "", errors.New("Android operation is unsupported")
	}
}

func androidDialNumber(number string) bool {
	digits := strings.TrimPrefix(number, "+")
	if len(digits) == 0 || len(digits) > 15 {
		return false
	}
	for i := range digits {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// am can exit successfully while reporting an unresolved intent or exception.
// Require its -W success status, and fail closed on diagnostics on either stream.
func androidActivityAccepted(out proc.Result) bool {
	stdout := string(out.Stdout)
	for _, text := range []string{stdout, string(out.Stderr)} {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "error") || strings.Contains(lower, "exception") {
			return false
		}
	}
	accepted := false
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Status:") {
			if strings.TrimSpace(strings.TrimPrefix(line, "Status:")) != "ok" {
				return false
			}
			accepted = true
		}
	}
	return accepted
}
