package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/speech"
)

type phoneReceiptDevice struct {
	state device.State
	err   error
}

func (*phoneReceiptDevice) ID() string                                   { return "phone" }
func (*phoneReceiptDevice) Kind() string                                 { return "phone" }
func (*phoneReceiptDevice) Capabilities() []device.Op                    { return []device.Op{device.Op("dial")} }
func (*phoneReceiptDevice) Describe() string                             { return "test phone" }
func (d *phoneReceiptDevice) Read(context.Context) (device.State, error) { return d.state, d.err }
func (d *phoneReceiptDevice) Apply(context.Context, device.Command) (device.State, error) {
	return d.state, d.err
}

type phoneClaimPlanner struct{}

func (phoneClaimPlanner) Name() string              { return "phone claim" }
func (phoneClaimPlanner) Available() (bool, string) { return true, "available" }
func (phoneClaimPlanner) Plan(context.Context, string, []device.Info) (brain.Plan, error) {
	return brain.Plan{Speak: "I called them and they answered.", Actions: []brain.Action{{Device: "phone", Op: device.Op("dial"), Args: map[string]any{"number": "+15550102020"}}}}, nil
}

func TestPhoneReceiptReplacesPlannerCompletionClaim(t *testing.T) {
	const receipt = "Dialer request sent. Review the number and tap Call on your phone to place the call."
	a := &Assistant{Brain: phoneClaimPlanner{}, Devices: device.NewRegistry()}
	a.Devices.Add(&phoneReceiptDevice{state: device.State{"receipt": receipt}})
	plan, err := a.HandleText(context.Background(), "dial +15550102020 on phone")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Speak != "phone: "+receipt {
		t.Fatalf("unsafe spoken result: %q", plan.Speak)
	}
}

func TestPhoneMissingReceiptAndFailureCannotReportSuccess(t *testing.T) {
	for _, state := range []device.State{nil, {}, {"receipt": ""}, {"receipt": " "}, {"receipt": true}} {
		a := &Assistant{Devices: device.NewRegistry()}
		a.Devices.Add(&phoneReceiptDevice{state: state})
		reply, err := a.ApplyActions(context.Background(), []brain.Action{{Device: "phone", Op: device.Op("dial")}})
		if err == nil || reply != "" {
			t.Fatalf("unverified result: %q, %v", reply, err)
		}
	}
	a := &Assistant{Devices: device.NewRegistry()}
	a.Devices.Add(&phoneReceiptDevice{state: device.State{"receipt": "success"}, err: errors.New("unauthorized")})
	if reply, err := a.ApplyActions(context.Background(), []brain.Action{{Device: "phone", Op: device.Op("dial")}}); err == nil || reply != "" {
		t.Fatalf("failed phone result: %q, %v", reply, err)
	}
}

func TestPhoneReceiptPreservesReadResultsAndProposalReceipt(t *testing.T) {
	a := &Assistant{Devices: device.NewRegistry(), Proposals: proposalStore(t)}
	a.Devices.Add(&phoneReceiptDevice{state: device.State{"receipt": "Review the dialer on your phone."}})
	a.Devices.Add(&readStateDevice{state: device.State{"state": "locked"}})
	plan, err := a.ApplyPlan(context.Background(), brain.Plan{
		Speak:    "The call is complete.",
		Actions:  []brain.Action{{Device: "front lock", Op: device.OpRead}, {Device: "phone", Op: device.Op("dial")}},
		Proposal: &brain.Proposal{Request: "support messages", Title: "Messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"front lock is locked.", "phone: Review the dialer", "saved your request for approval"} {
		if !strings.Contains(plan.Speak, want) {
			t.Fatalf("lost verified receipt %q: %q", want, plan.Speak)
		}
	}
	if strings.Contains(plan.Speak, "call is complete") {
		t.Fatalf("model claim escaped: %q", plan.Speak)
	}
}

func TestDeliveryPreservesVerifiedReceiptsNotPlannerClaims(t *testing.T) {
	const falseClaim = "The call is complete and the new feature is already installed."
	const direction = "angry"
	for _, scenario := range []struct {
		name     string
		actions  []brain.Action
		proposal *brain.Proposal
	}{
		{name: "phone", actions: []brain.Action{{Device: "phone", Op: device.Op("dial")}}},
		{name: "read", actions: []brain.Action{{Device: "front lock", Op: device.OpRead}}},
		{name: "proposal", proposal: &brain.Proposal{Request: "support messages"}},
		{name: "combined", actions: []brain.Action{{Device: "phone", Op: device.Op("dial")}, {Device: "front lock", Op: device.OpRead}}, proposal: &brain.Proposal{Request: "support messages"}},
	} {
		t.Run(direction+"/"+scenario.name, func(t *testing.T) {
			a := &Assistant{Devices: device.NewRegistry(), Proposals: proposalStore(t)}
			a.Devices.Add(&phoneReceiptDevice{state: device.State{"receipt": "Review the dialer on your phone."}})
			a.Devices.Add(&readStateDevice{state: device.State{"state": "locked"}})
			input := brain.Plan{Speak: falseClaim, Actions: scenario.actions, Proposal: scenario.proposal}
			plain, err := a.ApplyPlan(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			input.Speak = "[voice: " + direction + "] " + falseClaim
			styled, err := a.ApplyPlan(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			gotDirection, spoken := speech.SplitDelivery(styled.Speak)
			if gotDirection != direction || spoken != plain.Speak {
				t.Fatalf("delivery changed verified receipt: direction=%q spoken=%q; want direction=%q receipt=%q", gotDirection, spoken, direction, plain.Speak)
			}
			if spoken == "" || strings.Contains(spoken, falseClaim) {
				t.Fatalf("model claim survived receipt replacement: %q", styled.Speak)
			}
			next, err := a.ApplyPlan(context.Background(), brain.Plan{Speak: "An ordinary reply with [literal brackets]."})
			if err != nil {
				t.Fatal(err)
			}
			if next.Speak != "An ordinary reply with [literal brackets]." {
				t.Fatalf("delivery leaked into a later reply: %q", next.Speak)
			}
		})
	}
}
