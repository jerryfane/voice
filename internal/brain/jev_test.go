package brain

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/device"
)

func TestJevRoutesValidatedHighConfidenceAction(t *testing.T) {
	server := decisionServer(t, `{"answers":{"route":{"type":"choice","choice":"action_0","confidence":0.97,"probabilities":{"action_0":0.97,"agent":0.03}}},"model":"typesafe/jev-1.13","usage":{"input_tokens":20,"output_tokens":1}}`)
	t.Cleanup(server.Close)

	reached := false
	j := NewJev(server.URL, "typesafe/jev-1.13", "secret", 0.85, time.Second, &refusingPlanner{onCall: func() { reached = true }}, nil)
	inv := []device.Info{{ID: "spotify", Kind: "music", Capabilities: []string{string(device.OpPause)}}}
	p, err := j.Plan(context.Background(), "pause the music", inv)
	if err != nil {
		t.Fatal(err)
	}
	if reached {
		t.Fatal("high-confidence bounded action reached the agent")
	}
	if p.Source != "jev" || len(p.Actions) != 1 || p.Actions[0].Device != "spotify" || p.Actions[0].Op != device.OpPause {
		t.Fatalf("plan = %+v", p)
	}
}

func TestJevFallsBackOnLowConfidenceAndProviderFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "low confidence", status: http.StatusOK, body: `{"answers":{"route":{"type":"choice","choice":"action_0","confidence":0.4}},"model":"typesafe/jev-1.13","usage":{"input_tokens":20,"output_tokens":1}}`},
		{name: "provider failure", status: http.StatusServiceUnavailable, body: `{"error":{"message":"busy"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := decisionServerStatus(t, tt.status, tt.body)
			t.Cleanup(server.Close)
			reached := false
			j := NewJev(server.URL, "typesafe/jev-1.13", "secret", 0.85, time.Second, &refusingPlanner{onCall: func() { reached = true }}, nil)
			inv := []device.Info{{ID: "spotify", Kind: "music", Capabilities: []string{string(device.OpPause)}}}
			p, err := j.Plan(context.Background(), "pause something", inv)
			if err != nil {
				t.Fatal(err)
			}
			if !reached || p.Speak != "from the model" {
				t.Fatalf("fallback was not reached: %+v", p)
			}
		})
	}
}

func TestJevNeverOffersArgumentBearingActions(t *testing.T) {
	criteria, actions := routeChoices([]device.Info{{
		ID: "spotify", Kind: "music",
		Capabilities: []string{string(device.OpPause), string(device.OpVolume), string(device.OpPlay)},
	}})
	if len(actions) != 2 {
		t.Fatalf("offered actions = %d, want pause and argument-free play only", len(actions))
	}
	for key, action := range actions {
		if action.op == device.OpVolume {
			t.Fatalf("argument-bearing volume operation was offered as %q", key)
		}
	}
	if _, ok := criteria["agent"]; !ok {
		t.Fatal("agent fallback choice is missing")
	}
}

func decisionServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return decisionServerStatus(t, http.StatusOK, body)
}

func decisionServerStatus(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(status)
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Error(err)
		}
	}))
}
