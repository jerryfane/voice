package brain

import (
	"context"
	"testing"

	"github.com/jerryfane/voice/internal/device"
)

func TestMentioningDeviceActionDoesNotExecuteLocalWrite(t *testing.T) {
	inventory := []device.Info{{ID: "bedroom", Kind: "light", Capabilities: []string{"on", "off", "color", "mute"}}}
	for _, text := range []string{
		"do not turn on bedroom", "don't turn off bedroom", "if it gets dark turn on bedroom",
		"why did you turn on bedroom", "is bedroom red", "tell me how to mute bedroom",
	} {
		t.Run(text, func(t *testing.T) {
			delegated := false
			r := NewRules(&refusingPlanner{onCall: func() { delegated = true }})
			plan, err := r.Plan(context.Background(), text, inventory)
			if err != nil || !delegated || len(plan.Actions) != 0 {
				t.Fatalf("non-command triggered local action: %+v, %v", plan, err)
			}
		})
	}
	for _, text := range []string{"turn on bedroom", "please turn on the bedroom", "turn bedroom on"} {
		r := NewRules(&refusingPlanner{onCall: func() { t.Error("explicit command delegated") }})
		plan, err := r.Plan(context.Background(), text, inventory)
		if err != nil || len(plan.Actions) != 1 || plan.Actions[0].Device != "bedroom" || plan.Actions[0].Op != device.OpOn {
			t.Fatalf("explicit command lost: %+v, %v", plan, err)
		}
	}
}
