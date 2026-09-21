// Package brain turns a transcript into something to say and something to do.
//
// Two modes exist, and the distinction matters:
//
//   - "plan": the transcript plus a device inventory go to a stateless model,
//     which returns a strict JSON Plan.
//   - "agent": the transcript goes to a persistent, tool-capable agent session.
//     Voice still validates and executes configured local-device actions itself.
//
// Before either runs, a rules pass answers trivial local questions (time,
// date, "stop", "cancel") with no model call at all. Round-tripping a model
// for "what time is it?" is what makes assistants feel sluggish.
package brain

import (
	"context"

	"github.com/jerryfane/voice/internal/device"
)

// Action is one device command the planner wants executed.
type Action struct {
	Device string         `json:"device"`
	Op     device.Op      `json:"op"`
	Args   map[string]any `json:"args,omitempty"`
}

// Plan is the planner's complete response to one utterance.
type Plan struct {
	// Speak is what Voice says back. Empty means stay silent.
	Speak string `json:"speak"`
	// Actions run in order, after Speak is queued.
	Actions []Action `json:"actions,omitempty"`
	// Proposal arrives from the agent's structured response when the request
	// is beyond what the restricted account can do - Voice's own code or
	// configuration, installing software, anything needing the owner's
	// account. It is a REQUEST for the owner's approval, never an
	// authorization: nothing in it lets the agent act, and the agent cannot
	// approve its own. Nil is the normal case.
	Proposal *Proposal `json:"proposal,omitempty"`
	// Source records which layer produced this plan ("rules", "plan", "agent")
	// for logging; it is not part of the model's JSON contract.
	Source string `json:"-"`
}

// Proposal is a capability the agent was asked for and could not deliver.
// Answering such a request without recording it loses it: the owner asked for
// the thinking sound's volume to be configurable, the honest answer was that
// it could not be changed from that account, and nobody found out for days.
type Proposal struct {
	// Request is the owner's words, verbatim and on one line. A paraphrase is
	// the agent's reading of what was said rather than what was said.
	Request string `json:"request"`
	// Title is the agent's short interpretation, for a human scanning a list.
	Title string `json:"title"`
	// Scope is what the agent proposes doing.
	Scope string `json:"scope"`
	// Risks are the material risks of doing it.
	Risks string `json:"risks"`
}

// Planner converts a transcript into a Plan.
type Planner interface {
	Plan(ctx context.Context, transcript string, inventory []device.Info) (Plan, error)
	Name() string
	Available() (bool, string)
}
