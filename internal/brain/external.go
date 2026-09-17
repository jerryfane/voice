package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/herdr-voice/internal/device"
	"github.com/jerryfane/herdr-voice/internal/proc"
)

// External asks an arbitrary CLI model for a strict JSON plan.
type External struct {
	Engine  string
	Argv    []string
	Persona string
	Timeout time.Duration
	Mode    string
}

func NewExternal(name string, argv []string, persona string, timeout time.Duration, mode string) *External {
	return &External{name, argv, persona, timeout, mode}
}
func (e *External) Name() string { return e.Engine }
func (e *External) Available() (bool, string) {
	if len(e.Argv) == 0 {
		return false, "command is empty"
	}
	p, ok := proc.Which(e.Argv[0])
	if !ok {
		return false, e.Argv[0] + " not found on PATH"
	}
	return true, p
}
func (e *External) Plan(ctx context.Context, transcript string, inventory []device.Info) (Plan, error) {
	inv, _ := json.Marshal(inventory)
	prompt := fmt.Sprintf(`%s

You are the reasoning core inside a voice assistant. The user said: %q
Available local devices: %s
Return ONLY one JSON object with this exact shape:
{"speak":"short response spoken aloud","actions":[{"device":"configured id","op":"advertised capability","args":{}}]}
Rules: Use only listed devices and advertised capabilities. Never invent an ID. For color use args {"name":"red"} or integer r/g/b. For brightness use {"level":0..100}. If no local action is needed, actions is []. No markdown.`, e.Persona, transcript, string(inv))
	argv := proc.Expand(e.Argv, map[string]string{"prompt": prompt})
	stdin := []byte(nil)
	if !has(e.Argv, "{prompt}") {
		stdin = []byte(prompt)
	}
	r, err := proc.Run(ctx, argv, stdin, e.Timeout)
	if err != nil {
		return Plan{}, err
	}
	raw := strings.TrimSpace(string(r.Stdout))
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var p Plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Plan{}, fmt.Errorf("%s returned invalid plan JSON: %w (output: %.300s)", e.Engine, err, raw)
	}
	p.Source = e.Mode
	for _, a := range p.Actions {
		d, err := findInfo(inventory, a.Device)
		if err != nil {
			return Plan{}, err
		}
		ok := false
		for _, c := range d.Capabilities {
			if c == string(a.Op) {
				ok = true
				break
			}
		}
		if !ok {
			return Plan{}, fmt.Errorf("planner requested unsupported %s on %s", a.Op, a.Device)
		}
	}
	return p, nil
}
func has(a []string, s string) bool {
	for _, v := range a {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}
func findInfo(a []device.Info, id string) (device.Info, error) {
	for _, v := range a {
		if strings.EqualFold(v.ID, id) {
			return v, nil
		}
	}
	return device.Info{}, fmt.Errorf("planner requested unknown device %q", id)
}
