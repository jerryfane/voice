package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
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
