package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/proc"
)

// External asks an arbitrary CLI model for a validated JSON response.
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
	inv, err := json.Marshal(inventory)
	if err != nil {
		// The planner would otherwise be asked to act on an empty inventory
		// and confidently report that no such device exists.
		return Plan{}, fmt.Errorf("encoding the device inventory: %w", err)
	}
	prompt := e.prompt(transcript, string(inv))
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
func (e *External) prompt(transcript, inventory string) string {
	if e.Mode == "agent" {
		return fmt.Sprintf(`%s

This is the next turn in your one persistent Voice session.
The user said: %q
Available local devices: %s

Use your tools when they help. You may inspect or change files in your workspace, run commands, search the web, and use the browser. Do not call the voice executable to control listed local devices; request those through actions instead.

After completing the work, return ONLY one JSON object with this exact shape:
{"speak":"short natural response spoken aloud","actions":[{"device":"configured id","op":"advertised capability","args":{}}]}
Use only listed devices and advertised capabilities. Never invent a device ID. For color use args {"name":"red"} or integer r/g/b. For brightness use {"level":0..100}. For a music play action, omit args to start shuffled liked songs, or use {"query":"name","type":"track|album|artist|playlist"}; use resume for the paused item. Music volume uses integer args {"level":0..10}; volume_up and volume_down change one step. If no local device action is needed, actions is []. No markdown outside the JSON.`, e.Persona, transcript, inventory)
	}
	return fmt.Sprintf(`%s

You are the reasoning core inside a voice assistant. The user said: %q
Available local devices: %s
Return ONLY one JSON object with this exact shape:
{"speak":"short response spoken aloud","actions":[{"device":"configured id","op":"advertised capability","args":{}}]}
Rules: Use only listed devices and advertised capabilities. Never invent an ID. For color use args {"name":"red"} or integer r/g/b. For brightness use {"level":0..100}. For a music play action, omit args to start shuffled liked songs, or use {"query":"name","type":"track|album|artist|playlist"}; use resume for the paused item. Music volume uses integer args {"level":0..10}; volume_up and volume_down change one step. If no local action is needed, actions is []. No markdown.`, e.Persona, transcript, inventory)
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
