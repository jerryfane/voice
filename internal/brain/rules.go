package brain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/herdr-voice/internal/device"
)

// Rules handles deterministic common commands without an LLM round-trip and
// delegates everything else to Next.
type Rules struct {
	Next Planner
	Now  func() time.Time
}

func NewRules(next Planner) *Rules         { return &Rules{Next: next, Now: time.Now} }
func (r *Rules) Name() string              { return "rules -> " + r.Next.Name() }
func (r *Rules) Available() (bool, string) { return r.Next.Available() }
func (r *Rules) Plan(ctx context.Context, text string, inv []device.Info) (Plan, error) {
	n := strings.ToLower(strings.TrimSpace(text))
	now := r.Now()
	if strings.Contains(n, "what time") || n == "time" {
		return Plan{Speak: fmt.Sprintf("It is %s.", now.Format("3:04 PM")), Source: "rules"}, nil
	}
	if strings.Contains(n, "what date") || strings.Contains(n, "what day") || n == "date" {
		return Plan{Speak: fmt.Sprintf("It is %s.", now.Format("Monday, January 2")), Source: "rules"}, nil
	}
	if n == "stop" || n == "cancel" || n == "never mind" {
		return Plan{Speak: "Okay.", Source: "rules"}, nil
	}
	for _, d := range inv {
		id := strings.ToLower(d.ID)
		if !strings.Contains(n, id) {
			continue
		}
		if strings.Contains(n, "turn on") || strings.HasPrefix(n, "on ") {
			return actionPlan(d, device.OpOn, nil)
		}
		if strings.Contains(n, "turn off") || strings.HasPrefix(n, "off ") {
			return actionPlan(d, device.OpOff, nil)
		}
		if strings.Contains(n, "mute") {
			return actionPlan(d, device.OpMute, nil)
		}
		if strings.Contains(n, "volume up") || strings.Contains(n, "louder") {
			return actionPlan(d, device.OpVolumeUp, nil)
		}
		if strings.Contains(n, "volume down") || strings.Contains(n, "quieter") {
			return actionPlan(d, device.OpVolumeDown, nil)
		}
		for _, c := range []string{"red", "green", "blue", "purple", "orange", "yellow", "cyan", "pink", "white"} {
			if strings.Contains(n, c) {
				return actionPlan(d, device.OpColor, map[string]any{"name": c})
			}
		}
	}
	return r.Next.Plan(ctx, text, inv)
}
func actionPlan(d device.Info, op device.Op, args map[string]any) (Plan, error) {
	ok := false
	for _, c := range d.Capabilities {
		if c == string(op) {
			ok = true
			break
		}
	}
	if !ok {
		return Plan{}, fmt.Errorf("%s does not support %s", d.ID, op)
	}
	verb := strings.ReplaceAll(string(op), "_", " ")
	return Plan{Speak: fmt.Sprintf("Okay, %s %s.", verb, d.ID), Actions: []Action{{Device: d.ID, Op: op, Args: args}}, Source: "rules"}, nil
}
