package brain

import (
	"context"
	"testing"

	"github.com/jerryfane/voice/internal/device"
)

func quickPhone(id string) device.Info {
	return device.Info{ID: id, Kind: "phone", Capabilities: []string{
		string(device.OpWake), string(device.OpHome), string(device.OpSettings),
	}}
}

func TestPhoneQuickCommandsAvoidPlanner(t *testing.T) {
	for _, tc := range []struct {
		text string
		op   device.Op
	}{
		{"wake the phone", device.OpWake},
		{"wake up the phone", device.OpWake},
		{"Please, wake up the phone!", device.OpWake},
		{"wake phone please", device.OpWake},
		{"wake work-pixel", device.OpWake},
		{"wake up the work pixel", device.OpWake},
		{"go Home", device.OpHome},
		{"go to the Home screen", device.OpHome},
		{"go Home on the phone", device.OpHome},
		{"go to the Home screen on work_pixel", device.OpHome},
		{"Please go home on the work-pixel, please.", device.OpHome},
		{"open Settings", device.OpSettings},
		{"open Settings on the phone", device.OpSettings},
		{"open Android Settings", device.OpSettings},
		{"open Android Settings on the work pixel", device.OpSettings},
	} {
		t.Run(tc.text, func(t *testing.T) {
			r := NewRules(&refusingPlanner{onCall: func() { t.Fatal("simple command reached planner") }})
			p, err := r.Plan(context.Background(), tc.text, []device.Info{quickPhone("work-pixel")})
			if err != nil {
				t.Fatal(err)
			}
			if p.Source != "rules" || len(p.Actions) != 1 {
				t.Fatalf("expected one local action, got %+v", p)
			}
			a := p.Actions[0]
			if a.Device != "work-pixel" || a.Op != tc.op || len(a.Args) != 0 {
				t.Fatalf("wrong action: %+v", a)
			}
		})
	}
}

func TestPhoneQuickCommandsSelectExplicitPhone(t *testing.T) {
	for _, inv := range [][]device.Info{
		{quickPhone("personal"), quickPhone("work-pixel")},
		{quickPhone("work-pixel"), quickPhone("personal")},
	} {
		r := NewRules(&refusingPlanner{onCall: func() { t.Fatal("explicit phone reached planner") }})
		p, err := r.Plan(context.Background(), "open settings on work pixel", inv)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Actions) != 1 || p.Actions[0].Device != "work-pixel" || p.Actions[0].Op != device.OpSettings {
			t.Fatalf("wrong explicit target: %+v", p)
		}
	}
}

func TestPhoneQuickCommandsFallBack(t *testing.T) {
	phone := quickPhone("work-pixel")
	disabled := device.Info{ID: "other", Kind: "phone", Capabilities: []string{string(device.OpRead)}}
	for _, tc := range []struct {
		name string
		text string
		inv  []device.Info
	}{
		{"no phone", "open settings", nil},
		{"disabled", "open settings", []device.Info{disabled}},
		{"missing operation", "wake the phone", []device.Info{{ID: "work", Kind: "phone", Capabilities: []string{string(device.OpHome)}}}},
		{"two phones", "go home", []device.Info{phone, quickPhone("personal")}},
		{"two phones reversed", "go home", []device.Info{quickPhone("personal"), phone}},
		{"disabled second phone", "wake the phone", []device.Info{phone, disabled}},
		{"explicit disabled phone", "open settings on other", []device.Info{phone, disabled}},
		{"unknown target", "open settings on personal", []device.Info{phone}},
		{"non phone target", "open settings on tablet", []device.Info{phone, {ID: "tablet", Kind: "light", Capabilities: phone.Capabilities}}},
		{"generic name collision", "wake the phone", []device.Info{phone, {ID: "phone", Kind: "light"}}},
		{"implicit name collision", "go home", []device.Info{phone, {ID: "home", Kind: "light"}}},
		{"normalised collision", "open settings on work pixel", []device.Info{phone, quickPhone("work_pixel")}},
		{"non phone normalised collision", "open settings on work pixel", []device.Info{phone, {ID: "work_pixel", Kind: "light"}}},
		{"overlapping names", "wake work pixel", []device.Info{phone, quickPhone("pixel")}},
		{"negation", "don't wake the phone", []device.Info{phone}},
		{"negated settings", "do not open settings", []device.Info{phone}},
		{"conditional", "if the phone is asleep wake the phone", []device.Info{phone}},
		{"conditional suffix", "wake the phone if it is asleep", []device.Info{phone}},
		{"how question", "how do I open settings on the phone", []device.Info{phone}},
		{"explanation", "tell me how to go home", []device.Info{phone}},
		{"compound", "wake the phone and open settings", []device.Info{phone}},
		{"compound named phones", "wake work pixel and open settings on personal", []device.Info{phone, quickPhone("personal")}},
		{"negated named phones", "don't wake work pixel or personal", []device.Info{phone, quickPhone("personal")}},
		{"settings subtask", "open settings and enable bluetooth", []device.Info{phone}},
		{"settings section", "open settings on the phone and change the wallpaper", []device.Info{phone}},
		{"other app", "open YouTube on the phone", []device.Info{phone}},
		{"other named app", "open Spotify on work pixel", []device.Info{phone}},
		{"elaborate", "find the Wi Fi password in settings on the phone", []device.Info{phone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := NewRules(&refusingPlanner{onCall: func() { calls++ }})
			p, err := r.Plan(context.Background(), tc.text, tc.inv)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(p.Actions) != 0 || p.Source == "rules" {
				t.Fatalf("unsafe request did not fall back: calls=%d plan=%+v", calls, p)
			}
		})
	}
}
