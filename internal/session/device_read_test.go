package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
)

type readStateDevice struct {
	state device.State
	err   error
	calls int
}

func (*readStateDevice) ID() string                { return "front lock" }
func (*readStateDevice) Kind() string              { return "lock" }
func (*readStateDevice) Capabilities() []device.Op { return []device.Op{device.OpRead} }
func (d *readStateDevice) Apply(context.Context, device.Command) (device.State, error) {
	d.calls++
	return d.state, d.err
}
func (d *readStateDevice) Read(context.Context) (device.State, error) { return d.state, d.err }
func (*readStateDevice) Describe() string                             { return "read-only test lock" }

type readClaimPlanner struct{ proposal *brain.Proposal }

func (p readClaimPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	return brain.Plan{Speak: "The front lock is unlocked.", Actions: []brain.Action{{Device: "front lock", Op: device.OpRead}}, Proposal: p.proposal}, nil
}
func (readClaimPlanner) Name() string              { return "read claim planner" }
func (readClaimPlanner) Available() (bool, string) { return true, "available" }

func TestHandleTextSpeaksReadStateNotPlannerGuessAndPreservesProposal(t *testing.T) {
	lock := &readStateDevice{state: device.State{"state": "locked", "locked": true}}
	a := &Assistant{Brain: readClaimPlanner{proposal: &brain.Proposal{Request: "support another device", Title: "Another device"}}, Devices: device.NewRegistry(), Proposals: proposalStore(t)}
	a.Devices.Add(lock)
	plan, err := a.HandleText(context.Background(), "is the front lock locked")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Speak, "front lock is locked.") || strings.Contains(plan.Speak, "unlocked") {
		t.Fatalf("unverified planner claim escaped: %q", plan.Speak)
	}
	if !strings.Contains(plan.Speak, "owner for approval") {
		t.Fatalf("read state swallowed proposal notice: %q", plan.Speak)
	}
	if lock.calls != 1 {
		t.Fatalf("state read executed %d times", lock.calls)
	}
}

func TestReadFailureNeverReturnsGuessedReply(t *testing.T) {
	for _, state := range []device.State{nil, {"state": "unknown"}, {"state": "unavailable"}} {
		a := &Assistant{Devices: device.NewRegistry()}
		a.Devices.Add(&readStateDevice{state: state})
		reply, err := a.ApplyActions(context.Background(), []brain.Action{{Device: "front lock", Op: device.OpRead}})
		if err == nil || reply != "" {
			t.Fatalf("unverified read = %q, %v", reply, err)
		}
	}
	a := &Assistant{Devices: device.NewRegistry()}
	a.Devices.Add(&readStateDevice{err: errors.New("hub unavailable")})
	if reply, err := a.ApplyActions(context.Background(), []brain.Action{{Device: "front lock", Op: device.OpRead}}); err == nil || reply != "" {
		t.Fatalf("failed authenticated read = %q, %v", reply, err)
	}
}

func TestClimateReadOnlyNamesReportedUnits(t *testing.T) {
	state := device.State{"state": "heat", "temperature": 22.0, "current_temperature": 21.5}
	reply, err := deviceStateReply("office thermostat", state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, "Celsius") || strings.Contains(reply, "Fahrenheit") || !strings.Contains(reply, "21.5 degrees in its configured units") {
		t.Fatalf("assumed temperature unit: %q", reply)
	}
	state["temperature_unit"] = "°C"
	reply, err = deviceStateReply("office thermostat", state)
	if err != nil || !strings.Contains(reply, "22 degrees Celsius") {
		t.Fatalf("reported unit lost: %q, %v", reply, err)
	}
}
