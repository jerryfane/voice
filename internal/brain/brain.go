// Package brain turns a transcript into something to say and something to do.
//
// Two modes exist, and the distinction matters:
//
//   - "plan": the transcript plus a device inventory go to the model, which
//     returns a strict JSON Plan. herdr executes the actions itself. Fast,
//     predictable, and the model never gets shell access.
//   - "agent": the transcript is handed to a full agent session (omp/claude)
//     that may use its own tools, including the herdr CLI. Slower, unbounded
//     capability. Use when you want "figure it out", not "do this".
//
// Before either runs, a rules pass answers trivial local questions (time,
// date, "stop", "cancel") with no model call at all. Round-tripping a model
// for "what time is it?" is what makes assistants feel sluggish.
package brain

import (
	"context"

	"github.com/jerryfane/herdr-voice/internal/device"
)

// Action is one device command the planner wants executed.
type Action struct {
	Device string         `json:"device"`
	Op     device.Op      `json:"op"`
	Args   map[string]any `json:"args,omitempty"`
}

// Plan is the planner's complete response to one utterance.
type Plan struct {
	// Speak is what herdr says back. Empty means stay silent.
	Speak string `json:"speak"`
	// Actions run in order, after Speak is queued.
	Actions []Action `json:"actions,omitempty"`
	// Source records which layer produced this plan ("rules", "plan", "agent")
	// for logging; it is not part of the model's JSON contract.
	Source string `json:"-"`
}

// Planner converts a transcript into a Plan.
type Planner interface {
	Plan(ctx context.Context, transcript string, inventory []device.Info) (Plan, error)
	Name() string
	Available() (bool, string)
}
