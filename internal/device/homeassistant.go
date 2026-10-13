package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

var haEntityID = regexp.MustCompile(`^(light|switch|climate|lock)\.[a-z0-9_]+$`)

// HomeAssistant exposes only an owner-selected entity and operation allowlist.
// It never offers arbitrary service calls, state injection, or lock writes.
// Protocol: https://developers.home-assistant.io/docs/api/rest/
type HomeAssistant struct {
	id, entity, domain string
	ops                []Op
	api                *haAPI
	err                error
}

type haAPI struct {
	base, tokenEnv string
	client         *http.Client
}

type haState struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

// NewHomeAssistant does not perform network I/O. Configuration or credential
// failures are reported by Read/Apply and therefore by voice doctor.
func NewHomeAssistant(cfg config.HomeAssistant, entity config.HomeAssistantEntity) *HomeAssistant {
	d := &HomeAssistant{id: entity.ID, entity: entity.EntityID}
	d.api, d.err = newHAAPI(cfg)
	if !haEntityID.MatchString(entity.EntityID) {
		d.err = errors.New("Home Assistant entity must be an explicit light, switch, climate or lock entity")
		return d
	}
	d.domain = strings.SplitN(entity.EntityID, ".", 2)[0]
	d.ops = []Op{OpRead}
	for _, raw := range entity.Operations {
		op := Op(raw)
		allowed := op == OpRead || ((d.domain == "light" || d.domain == "switch") && (op == OpOn || op == OpOff || op == OpToggle)) ||
			(d.domain == "light" && (op == OpBrightness || op == OpColor)) ||
			(d.domain == "climate" && (op == OpTemperature || op == OpHVACMode))
		if !allowed {
			d.err = errors.New("Home Assistant operation is not allowed for this entity domain")
			d.ops = nil
			return d
		}
		if !Supports(d, op) {
			d.ops = append(d.ops, op)
		}
	}
	return d
}

func newHAAPI(cfg config.HomeAssistant) (*haAPI, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Home Assistant URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	tokenEnv := cfg.TokenEnv
	if tokenEnv == "" {
		tokenEnv = "VOICE_HA_TOKEN"
	}
	transport := &http.Transport{DialContext: dialHALocal, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &haAPI{base: strings.TrimRight(cfg.URL, "/"), tokenEnv: tokenEnv, client: &http.Client{
		Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") },
	}}, nil
}

// Resolve and validate each connection, including DNS changes; never send the
// bearer token to a public address, a proxy, or a redirect target.
func dialHALocal(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid Home Assistant address")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("cannot resolve Home Assistant local address")
	}
	for _, ip := range ips {
		if !(ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast()) {
			return nil, errors.New("Home Assistant endpoint must resolve only to local addresses")
		}
	}
	for _, ip := range ips {
		h := ip.IP.String()
		if ip.Zone != "" {
			h += "%" + ip.Zone
		}
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(h, port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("cannot connect to Home Assistant local endpoint")
}

func (a *haAPI) request(ctx context.Context, method, path string, body any, out any) error {
	token := os.Getenv(a.tokenEnv)
	if strings.TrimSpace(token) == "" {
		return errors.New("Home Assistant token environment variable is unset")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("invalid Home Assistant request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Home Assistant request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Home Assistant request failed (local connection, TLS, timeout or redirect)")
	}
	defer resp.Body.Close() // discard: the response is read-only and request errors are reported below.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never echo response text: integrations may include secrets in errors.
		return fmt.Errorf("Home Assistant HTTP %d", resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return errors.New("invalid or oversized Home Assistant response")
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("invalid Home Assistant JSON response")
	}
	return nil
}

func (d *HomeAssistant) ID() string         { return d.id }
func (d *HomeAssistant) Kind() string       { return d.domain }
func (d *HomeAssistant) Capabilities() []Op { return d.ops }
func (d *HomeAssistant) Describe() string {
	return "Home Assistant " + d.domain + " (explicit allowlist; reported state)"
}

func (d *HomeAssistant) read(ctx context.Context) (haState, error) {
	var state haState
	if d.err != nil {
		return state, d.err
	}
	if err := d.api.request(ctx, http.MethodGet, "/api/states/"+d.entity, nil, &state); err != nil {
		return state, err
	}
	if state.EntityID != d.entity {
		return haState{}, errors.New("Home Assistant returned a different entity")
	}
	if state.State == "" || state.State == "unknown" || state.State == "unavailable" {
		return haState{}, errors.New("Home Assistant entity state is unavailable or unknown")
	}
	known := false
	switch d.domain {
	case "light", "switch":
		known = state.State == "on" || state.State == "off"
	case "lock":
		known = haHas([]string{"locked", "unlocked", "locking", "unlocking", "jammed", "open", "opening"}, state.State)
	case "climate":
		known = haHas([]string{"off", "heat", "cool", "heat_cool", "auto", "dry", "fan_only"}, state.State)
	}
	if !known {
		return haState{}, errors.New("Home Assistant returned an unrecognized domain state")
	}
	return state, nil
}

// snapshot intentionally excludes names, locations, users and integration data.
func (d *HomeAssistant) snapshot(s haState) State {
	out := State{"state": s.State}
	switch d.domain {
	case "light", "switch":
		out["power"] = s.State
	case "lock":
		if s.State == "locked" || s.State == "unlocked" {
			out["locked"] = s.State == "locked"
		}
	case "climate":
		out["hvac_mode"] = s.State
	}
	if d.domain == "climate" {
		if unit, ok := s.Attributes["temperature_unit"].(string); ok && (unit == "°C" || unit == "°F") {
			out["temperature_unit"] = unit
		}
	}
	keys := []string{}
	if d.domain == "light" {
		keys = []string{"brightness"}
	}
	if d.domain == "climate" {
		keys = []string{"temperature", "current_temperature"}
	}
	for _, key := range keys {
		if n, ok := s.Attributes[key].(float64); ok {
			if key == "brightness" {
				n = math.Round(n * 100 / 255)
			}
			out[key] = n
		}
	}
	if d.domain == "light" {
		if rgb, ok := s.Attributes["rgb_color"].([]any); ok && len(rgb) == 3 {
			var values [3]float64
			valid := true
			for i, value := range rgb {
				n, ok := value.(float64)
				if !ok || n < 0 || n > 255 {
					valid = false
					break
				}
				values[i] = n
			}
			if valid {
				out["rgb_color"] = values
			}
		}
	}
	return out
}

func (d *HomeAssistant) Read(ctx context.Context) (State, error) {
	s, err := d.read(ctx)
	if err != nil {
		return nil, err
	}
	return d.snapshot(s), nil
}

func (d *HomeAssistant) Apply(ctx context.Context, cmd Command) (State, error) {
	if d.err != nil {
		return nil, d.err
	}
	if !Supports(d, cmd.Op) {
		return nil, errors.New("Home Assistant operation is not allowlisted")
	}
	if cmd.Op == OpRead {
		return d.Read(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	before, err := d.read(ctx)
	if err != nil {
		return nil, err
	}
	service, payload, matches, err := d.action(cmd, before)
	if err != nil {
		return nil, err
	}
	payload["entity_id"] = d.entity
	if err := d.api.request(ctx, http.MethodPost, "/api/services/"+d.domain+"/"+service, payload, nil); err != nil {
		return nil, err
	}
	// Service acceptance is not proof of effect; verify the selected entity,
	// not the unrelated states that HA may return from the service call.
	verify, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		after, err := d.read(verify)
		if err != nil {
			return nil, fmt.Errorf("Home Assistant command accepted but state not verified: %w", err)
		}
		if matches(after) {
			return d.snapshot(after), nil
		}
		timer.Reset(150 * time.Millisecond)
		select {
		case <-verify.Done():
			timer.Stop()
			return nil, errors.New("Home Assistant command accepted but requested state was not verified")
		case <-timer.C:
		}
	}
}

func haNumber(args map[string]any, key string) (float64, error) {
	var n float64
	switch value := args[key].(type) {
	case float64:
		n = value
	case int:
		n = float64(value)
	default:
		return 0, fmt.Errorf("%s requires a numeric value", key)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s requires a finite value", key)
	}
	return n, nil
}

func haStrings(v any) []string {
	var out []string
	if values, ok := v.([]any); ok {
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func haHas(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (d *HomeAssistant) action(cmd Command, before haState) (string, map[string]any, func(haState) bool, error) {
	payload := map[string]any{}
	var service string
	var matches func(haState) bool
	switch cmd.Op {
	case OpOn, OpOff, OpToggle:
		if before.State != "on" && before.State != "off" {
			return "", nil, nil, errors.New("entity does not report on/off state")
		}
		target := string(cmd.Op)
		if cmd.Op == OpToggle {
			target = "on"
			if before.State == "on" {
				target = "off"
			}
		}
		service = "turn_" + target
		matches = func(s haState) bool { return s.State == target }
	case OpBrightness:
		modes := haStrings(before.Attributes["supported_color_modes"])
		capable := false
		for _, mode := range modes {
			if mode != "onoff" && mode != "unknown" {
				capable = true
			}
		}
		level, err := haNumber(cmd.Args, "level")
		if err != nil || level < 0 || level > 100 || math.Trunc(level) != level {
			return "", nil, nil, errors.New("brightness requires an integer level from 0 to 100")
		}
		if !capable {
			return "", nil, nil, errors.New("light does not report brightness support")
		}
		service = "turn_on"
		payload["brightness_pct"] = level
		matches = func(s haState) bool {
			if level == 0 {
				return s.State == "off"
			}
			actual, ok := s.Attributes["brightness"].(float64)
			return s.State == "on" && ok && math.Abs(actual-math.Round(level*255/100)) <= 1
		}
	case OpColor:
		modes := haStrings(before.Attributes["supported_color_modes"])
		if !haHas(modes, "rgb") && !haHas(modes, "rgbw") && !haHas(modes, "rgbww") && !haHas(modes, "hs") && !haHas(modes, "xy") {
			return "", nil, nil, errors.New("light does not report color support")
		}
		if _, named := cmd.Args["name"]; !named {
			for _, key := range []string{"r", "g", "b"} {
				n, err := haNumber(cmd.Args, key)
				if err != nil || n < 0 || n > 255 || math.Trunc(n) != n {
					return "", nil, nil, errors.New("color requires a name or integer r/g/b from 0 to 255")
				}
			}
		}
		r, g, b, err := colorArgs(cmd.Args)
		if err != nil {
			return "", nil, nil, err
		}
		rgb := []int{int(r), int(g), int(b)}
		payload["rgb_color"], service = rgb, "turn_on"
		matches = func(s haState) bool {
			actual, ok := s.Attributes["rgb_color"].([]any)
			if s.State != "on" || !ok || len(actual) != 3 {
				return false
			}
			for i, want := range rgb {
				got, ok := actual[i].(float64)
				if !ok || math.Abs(got-float64(want)) > 3 {
					return false
				}
			}
			return true
		}
	case OpTemperature:
		value, err := haNumber(cmd.Args, "temperature")
		min, minOK := before.Attributes["min_temp"].(float64)
		max, maxOK := before.Attributes["max_temp"].(float64)
		features, _ := before.Attributes["supported_features"].(float64)
		if err != nil || !minOK || !maxOK || value < min || value > max || int(features)&1 == 0 || before.State == "heat_cool" {
			return "", nil, nil, errors.New("temperature requires a supported single setpoint within the thermostat's reported bounds and units")
		}
		service, payload["temperature"] = "set_temperature", value
		matches = func(s haState) bool {
			actual, ok := s.Attributes["temperature"].(float64)
			return ok && math.Abs(actual-value) < 0.01
		}
	case OpHVACMode:
		mode, ok := cmd.Args["mode"].(string)
		if !ok || !haHas([]string{"off", "heat", "cool", "heat_cool", "auto", "dry", "fan_only"}, mode) || !haHas(haStrings(before.Attributes["hvac_modes"]), mode) {
			return "", nil, nil, errors.New("HVAC mode is not supported by this thermostat")
		}
		service, payload["hvac_mode"] = "set_hvac_mode", mode
		matches = func(s haState) bool { return s.State == mode }
	default:
		return "", nil, nil, errors.New("unsupported Home Assistant operation")
	}
	return service, payload, matches, nil
}

// HomeAssistantCandidate is deliberately location/attribute-free. Discovery
// grants no authority; only a subsequent owner config edit can allow control.
type HomeAssistantCandidate struct {
	EntityID string `json:"entity_id"`
	Domain   string `json:"domain"`
	Policy   string `json:"policy"`
}

// DiscoverHomeAssistant authenticates and reads only the explicit configured
// endpoint. It never scans the LAN, invokes services or imports entities.
func DiscoverHomeAssistant(ctx context.Context, cfg config.HomeAssistant) ([]HomeAssistantCandidate, error) {
	api, err := newHAAPI(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var status map[string]any
	if err := api.request(ctx, http.MethodGet, "/api/", nil, &status); err != nil {
		return nil, err
	}
	var states []haState
	if err := api.request(ctx, http.MethodGet, "/api/states", nil, &states); err != nil {
		return nil, err
	}
	out := make([]HomeAssistantCandidate, 0)
	for _, state := range states {
		if !haEntityID.MatchString(state.EntityID) {
			continue
		}
		domain := strings.SplitN(state.EntityID, ".", 2)[0]
		policy := "read-only until owner allowlists operations; approve only safe loads"
		if domain == "lock" {
			policy = "status only; no lock/unlock support"
		}
		if domain == "climate" {
			policy = "read-only by default; explicitly allowlist temperature/hvac_mode only for compatible safe hardware"
		}
		out = append(out, HomeAssistantCandidate{EntityID: state.EntityID, Domain: domain, Policy: policy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EntityID < out[j].EntityID })
	return out, nil
}
