package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/device"
)

const maxDecisionResponse = 1 << 20

type decisionHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type routeLogger interface {
	Printf(string, ...any)
}

type decisionAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Jev asks one typed-choice question for bounded, no-argument device actions.
// Anything ambiguous, low-confidence, or requiring free-form arguments goes to
// Next unchanged.
type Jev struct {
	Endpoint   string
	Model      string
	APIKey     string
	Confidence float64
	Timeout    time.Duration
	Next       Planner
	Client     decisionHTTPClient
	Logger     routeLogger
}

func NewJev(endpoint, model, apiKey string, confidence float64, timeout time.Duration, next Planner, logger routeLogger) *Jev {
	return &Jev{Endpoint: endpoint, Model: model, APIKey: apiKey, Confidence: confidence, Timeout: timeout, Next: next, Client: http.DefaultClient, Logger: logger}
}

func (j *Jev) Name() string { return "jev -> " + j.Next.Name() }
func (j *Jev) Available() (bool, string) {
	ok, why := j.Next.Available()
	if strings.TrimSpace(j.APIKey) == "" {
		return ok, "Jev disabled: API key is not set; fallback: " + why
	}
	return ok, "Jev configured; fallback: " + why
}

func (j *Jev) Plan(ctx context.Context, transcript string, inventory []device.Info) (Plan, error) {
	if strings.TrimSpace(j.APIKey) == "" {
		j.logFallback("missing_credentials", 0, "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	criteria, actions := routeChoices(inventory)
	request := struct {
		Model     string                 `json:"model"`
		State     any                    `json:"state"`
		Questions map[string]interface{} `json:"questions"`
	}{
		Model: j.Model,
		State: map[string]any{
			"transcript": transcript,
			"devices":    inventory,
		},
		Questions: map[string]interface{}{
			"route": map[string]any{
				"type":         "choice",
				"instructions": "Choose exactly one safe route. Select a device action only for a direct, unambiguous request that needs no missing argument. Otherwise choose agent.",
				"criteria":     criteria,
			},
		},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Plan{}, fmt.Errorf("encode Jev request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, j.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Plan{}, err
	}
	req.Header.Set("Authorization", "Bearer "+j.APIKey)
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := j.Client.Do(req)
	if err != nil {
		j.logFallback("request_failed", elapsedRouteMS(started), "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDecisionResponse+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		j.logFallback("response_failed", elapsedRouteMS(started), "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	if len(raw) > maxDecisionResponse || resp.StatusCode != http.StatusOK {
		j.logFallback("http_"+fmt.Sprint(resp.StatusCode), elapsedRouteMS(started), "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	var out struct {
		Answers map[string]decisionAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		j.logFallback("malformed_response", elapsedRouteMS(started), "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	answer, ok := out.Answers["route"]
	if !ok || answer.Type != "choice" || answer.Choice == "" {
		j.logFallback("missing_choice", elapsedRouteMS(started), "")
		return j.Next.Plan(ctx, transcript, inventory)
	}
	if answer.Choice == "agent" {
		j.logChoiceFallback("agent", elapsedRouteMS(started), answer)
		return j.Next.Plan(ctx, transcript, inventory)
	}
	action, ok := actions[answer.Choice]
	if !ok {
		j.logChoiceFallback("unknown_choice", elapsedRouteMS(started), answer)
		return j.Next.Plan(ctx, transcript, inventory)
	}
	if answer.Confidence < j.Confidence {
		j.logChoiceFallback("low_confidence", elapsedRouteMS(started), answer)
		return j.Next.Plan(ctx, transcript, inventory)
	}
	p, err := actionPlan(action.info, action.op, nil)
	if err != nil {
		j.logChoiceFallback("invalid_action", elapsedRouteMS(started), answer)
		return j.Next.Plan(ctx, transcript, inventory)
	}
	p.Source = "jev"
	if j.Logger != nil {
		j.Logger.Printf("stage=route state=local source=jev ms=%.3f choice=%q confidence=%.4f probabilities=%v", elapsedRouteMS(started), answer.Choice, answer.Confidence, answer.Probabilities)
	}
	return p, nil
}

type routeAction struct {
	info device.Info
	op   device.Op
}

func routeChoices(inventory []device.Info) (map[string]string, map[string]routeAction) {
	criteria := map[string]string{
		"agent": "Any conversational, informational, ambiguous, named-search, or argument-bearing request; any request not exactly covered by another choice.",
	}
	actions := make(map[string]routeAction)
	n := 0
	for _, info := range inventory {
		for _, capability := range info.Capabilities {
			op := device.Op(capability)
			if !jevSafeWithoutArgs(op) {
				continue
			}
			key := fmt.Sprintf("action_%d", n)
			n++
			criteria[key] = fmt.Sprintf("Direct request to %s %q using operation %q, with no additional argument or named content.", info.Kind, info.ID, op)
			actions[key] = routeAction{info: info, op: op}
		}
	}
	return criteria, actions
}

func jevSafeWithoutArgs(op device.Op) bool {
	switch op {
	case device.OpOn, device.OpOff, device.OpToggle, device.OpVolumeUp, device.OpVolumeDown,
		device.OpMute, device.OpPlay, device.OpResume, device.OpPause, device.OpNext, device.OpPrevious:
		return true
	default:
		return false
	}
}

func (j *Jev) logFallback(reason string, ms float64, choice string) {
	if j.Logger == nil {
		return
	}
	j.Logger.Printf("stage=route state=agent source=jev ms=%.3f choice=%q reason=%s", ms, choice, reason)
}

func (j *Jev) logChoiceFallback(reason string, ms float64, answer decisionAnswer) {
	if j.Logger == nil {
		return
	}
	j.Logger.Printf("stage=route state=agent source=jev ms=%.3f choice=%q confidence=%.4f probabilities=%v reason=%s", ms, answer.Choice, answer.Confidence, answer.Probabilities, reason)
}

func elapsedRouteMS(started time.Time) float64 {
	if started.IsZero() {
		return 0
	}
	return float64(time.Since(started).Microseconds()) / 1000
}
