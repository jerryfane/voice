// Package session connects audio, VAD, speech, reasoning and devices into the
// wake-gated assistant loop.
package session

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/feedback"
	"github.com/jerryfane/voice/internal/health"
	"github.com/jerryfane/voice/internal/proposal"
	"github.com/jerryfane/voice/internal/speech"
	"github.com/jerryfane/voice/internal/timer"
	"github.com/jerryfane/voice/internal/vad"
	"github.com/jerryfane/voice/internal/wake"
)

// Assistant is one configured Voice runtime.
type Assistant struct {
	Recorder audio.Recorder
	Player   audio.Player
	VAD      vad.Segmenter
	// WakeSTT is an optional local-only privacy gate. When set, it must find a
	// configured wake phrase before STT may send the same audio to a remote
	// primary. Nil preserves the single-transcriber local pipeline.
	WakeSTT speech.Transcriber
	STT     speech.Transcriber
	// Vocabulary is applied only to commands after raw wake authorization.
	Vocabulary *speech.Vocabulary
	TTS        speech.Synthesizer
	Brain      brain.Planner
	// Front is an optional text-only conversational front desk. It cannot
	// execute a device action or write to the persistent agent.
	Front   brain.FrontDesk
	Devices *device.Registry
	// Feedback acknowledges an accepted wake phrase locally. A nil Notifier
	// disables light and sound without changing command handling.
	Feedback *feedback.Notifier
	// Timers runs local timers. A nil Scheduler means the timer vocabulary
	// falls through to the planner, as it did before timers existed.
	Timers *timer.Scheduler
	// Proposals records requests this account cannot carry out, so the owner
	// can approve them later. A nil Store disables recording without changing
	// command handling.
	Proposals *proposal.Store
	// Missed are timers that came due while Voice was not running; Run
	// reports them once at startup.
	Missed      []timer.Timer
	WakePhrases []string
	WakeFuzz    float64
	// Zero turns (the default) keeps wake-free in-flight interaction disabled.
	InFlightWindow time.Duration
	InFlightTurns  int
	// ConversationWindow/Turns opt into post-answer follow-ups. The deadline
	// starts after the first answer and never extends on wake-free input.
	ConversationWindow time.Duration
	ConversationTurns  int
	// Now is the clock used for timer arithmetic. Nil means time.Now.
	Now    func() time.Time
	Logger *log.Logger
	// Health is optional content-free live diagnostics, set before Run.
	Health        *health.Monitor
	HealthBudgets HealthBudgets
}

func (a *Assistant) logf(f string, v ...any) {
	if a.Logger != nil {
		a.Logger.Printf(f, v...)
	}
}

// HandleText is the non-voice entry point used by the CLI, agents and tests.
// It runs the exact same planner and device execution path as a spoken command.
func (a *Assistant) HandleText(ctx context.Context, text string) (p brain.Plan, err error) {
	ctx, done := a.Health.Begin(ctx, "plan", a.HealthBudgets.plan())
	defer func() { done(err) }()
	started := time.Now()
	p, err = a.Brain.Plan(ctx, strings.TrimSpace(text), a.Devices.Infos())
	if err != nil {
		return p, err
	}
	p, err = a.ApplyPlan(ctx, p)
	if err != nil {
		return p, err
	}
	if p.Source == "rules" {
		a.logf("stage=route state=local source=rules ms=%.3f", sinceMS(started))
	}
	return p, nil
}

// ApplyPlan is shared by spoken/text requests and the structured execute CLI.
// Proposal receipts come from durable state, never the model's handoff claims.
func (a *Assistant) ApplyPlan(ctx context.Context, p brain.Plan) (brain.Plan, error) {
	if err := ctx.Err(); err != nil {
		return p, err
	}
	if p.Proposal != nil {
		p.Speak = a.recordProposal(p)
	}
	readReply, err := a.ApplyActions(ctx, p.Actions)
	if err != nil {
		return p, err
	}
	if readReply != "" {
		if p.Proposal != nil {
			p.Speak = withNote(readReply, p.Speak)
		} else {
			p.Speak = readReply
		}
	}
	return p, nil
}

// recordProposal acknowledges only a persisted request, not owner notification
// or implementation. Failure must not retain an unverified model promise.
func (a *Assistant) recordProposal(p brain.Plan) string {
	if a.Proposals == nil {
		return "I couldn't save that request because work approvals are disabled. No work has been started."
	}
	request := strings.TrimSpace(p.Proposal.Request)
	if request == "" {
		return "I couldn't save an empty work request. Please tell me what you want changed."
	}
	stored, created, err := a.Proposals.Record(proposal.Proposal{
		Request: request,
		Title:   strings.TrimSpace(p.Proposal.Title),
		Scope:   strings.TrimSpace(p.Proposal.Scope),
		Risks:   strings.TrimSpace(p.Proposal.Risks),
	}, a.now())
	if err != nil {
		a.logf("stage=proposal state=failed request=%q err=%v", request, err)
		return "I couldn't save your request for approval. No work has been started."
	}
	a.logf("stage=proposal state=ok id=%s created=%v status=%s occurrences=%d", stored.ID, created, stored.Status, stored.Occurrences)
	return proposalNote(stored)
}

// Each state acknowledges only the boundary already crossed.
func proposalNote(stored proposal.Proposal) string {
	switch stored.Status {
	case proposal.Declined:
		return "That request was declined. No new work has been started."
	case proposal.Approved:
		return "Your request is approved and waiting for the implementation task to start."
	case proposal.IssueCreated:
		return "An issue has been filed for your approved request. The implementation task hasn't started yet."
	case proposal.Implementing:
		return "The implementation task has been started for your approved request; it isn't marked complete yet."
	case proposal.Completed:
		return "That request is marked complete."
	case proposal.Failed:
		return "That work request failed. It needs attention before it can continue."
	case proposal.Expired:
		return "That approval request expired. No new work has been started."
	default:
		return "I've saved your request for approval. Work won't start until you approve it."
	}
}

// withNote appends a sentence to a spoken reply, ending the reply first so
// the two do not run together when the model leaves off the full stop.
func withNote(speak, note string) string {
	speak = strings.TrimSpace(speak)
	if speak == "" {
		return note
	}
	if !strings.ContainsAny(speak[len(speak)-1:], ".!?") {
		speak += "."
	}
	return speak + " " + note
}

// ApplyActions validates and executes an already-produced structured plan.
// It returns descriptions of authenticated read results, which replace any
// planner-guessed state before microphone or CLI replies are spoken.
func (a *Assistant) ApplyActions(ctx context.Context, actions []brain.Action) (string, error) {
	var reads strings.Builder
	for _, act := range actions {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dev, err := a.Devices.Get(act.Device)
		if err != nil {
			return "", err
		}
		if !device.Supports(dev, act.Op) {
			return "", fmt.Errorf("%s does not support %s", dev.ID(), act.Op)
		}
		state, err := dev.Apply(ctx, device.Command{Op: act.Op, Args: act.Args})
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", act.Device, act.Op, err)
		}
		a.logf("action device=%s op=%s state=%v", act.Device, act.Op, state)
		if act.Op == device.OpRead {
			reply, err := deviceStateReply(dev.ID(), state)
			if err != nil {
				return "", err
			}
			if reads.Len() > 0 {
				reads.WriteByte(' ')
			}
			reads.WriteString(reply)
		}
	}
	return reads.String(), nil
}

func deviceStateReply(id string, state device.State) (string, error) {
	value, ok := state["state"].(string)
	if !ok || value == "" || value == "unknown" || value == "unavailable" {
		return "", fmt.Errorf("%s did not return a known state", id)
	}
	reply := fmt.Sprintf("%s is %s.", id, strings.ReplaceAll(value, "_", " "))
	unit, _ := state["temperature_unit"].(string)
	switch unit {
	case "°C":
		unit = " degrees Celsius"
	case "°F":
		unit = " degrees Fahrenheit"
	default:
		unit = " degrees in its configured units"
	}
	if temperature, ok := state["current_temperature"].(float64); ok {
		reply += fmt.Sprintf(" The current temperature is %g%s.", temperature, unit)
	}
	if temperature, ok := state["temperature"].(float64); ok {
		reply += fmt.Sprintf(" The target temperature is %g%s.", temperature, unit)
	}
	return reply, nil
}

const playbackLeadIn = 250 * time.Millisecond

type conversationGate interface {
	SetInFlightWindow(time.Time, int)
	Accepts(vad.Utterance) bool
	Suspend()
	ResumeAfter(time.Duration)
}

const playbackTail = 200 * time.Millisecond

// Speak synthesizes and plays one response.
func (a *Assistant) Speak(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if gate, ok := a.VAD.(conversationGate); ok {
		gate.Suspend()
		defer gate.ResumeAfter(playbackTail)
	}
	started := time.Now()
	synthCtx, synthDone := a.Health.Begin(ctx, "synthesize", a.HealthBudgets.synthesize())
	wav, err := a.TTS.Synthesize(synthCtx, text)
	if err == nil {
		err = synthCtx.Err()
	}
	synthDone(err)
	if err != nil {
		a.logf("stage=synthesize state=failed ms=%.3f err=%v", sinceMS(started), err)
		return err
	}
	a.logf("stage=synthesize state=ok ms=%.3f", sinceMS(started))
	wav, err = audio.PrependWAVSilence(wav, playbackLeadIn)
	if err != nil {
		return fmt.Errorf("pad speech lead-in: %w", err)
	}
	started = time.Now()
	playCtx, playDone := a.Health.Begin(ctx, "playback", 5*time.Minute)
	err = a.Player.PlayWAV(playCtx, wav)
	if err == nil {
		err = playCtx.Err()
	}
	playDone(err)
	if err != nil {
		a.logf("stage=playback state=failed ms=%.3f err=%v", sinceMS(started), err)
	} else {
		a.logf("stage=playback state=ok ms=%.3f", sinceMS(started))
	}
	return err
}

// Run listens until ctx is cancelled. Wake-free input is accepted only within
// an explicitly enabled, locally authorized time and turn budget.
func (a *Assistant) Run(ctx context.Context) error {
	ctx, cancelCapture := context.WithCancel(ctx)
	defer cancelCapture()
	if observer, ok := a.VAD.(interface {
		SetHealthObserver(interface {
			WakeProgress()
			WakeDetected()
		})
	}); ok && a.Health != nil {
		observer.SetHealthObserver(a.Health)
	}
	if gate, ok := a.VAD.(interface{ HealthState() string }); ok {
		a.Health.SetWakeGate(gate.HealthState)
	}
	a.Health.WakeState("starting")
	a.Health.LoopProgress()
	managed, hasManaged := a.VAD.(vad.ManagedSegmenter)
	if hasManaged {
		_, startupDone := a.Health.Begin(ctx, "wake_start", time.Minute)
		startupErr := managed.Start()
		startupDone(startupErr)
		if err := startupErr; err != nil {
			a.Health.WakeState("failed")
			a.Health.Failed("wake", "startup")
			a.logf("stage=wake state=failed source=streaming err=%v", err)
			return fmt.Errorf("start wake detector: %w", err)
		}
		a.logf("stage=wake state=ready source=streaming detector=%q", managed.Name())
		a.Health.WakeState("ready")
		a.Health.WakeProgress()
	}
	if !hasManaged {
		a.Health.WakeState("unverified")
	}
	// Establish a known indicator state at startup and leave it idle however
	// Run exits: cancellation, shutdown or a spoken "turn off".
	a.Feedback.Restore()
	defer a.Feedback.Restore()
	// Each stage of the pipeline logs its own outcome. A device that hears
	// nothing and a device whose transcriber cannot start produced the
	// identical journal - one log line after a *successful* transcription and
	// nothing before it - which is how a broken speech engine masqueraded as a
	// dead microphone for a whole boot. Silence should now name the stage that
	// stopped: capture, segment, transcribe, or wake.
	a.logf("stage=capture state=starting device=%q", a.Recorder.Describe())
	pcm, recErr := a.Recorder.Stream(ctx)
	// Every exit, including spoken shutdown or a capture failure, must let
	// the recorder release the speakerphone's microphone/off-hook state.
	defer func() {
		cancelCapture()
		a.awaitCaptureStop(pcm)
	}()
	segmentPCM := pcm
	if a.Health != nil {
		segmentPCM = audio.ObserveStream(ctx, pcm, a.Health.Capture, a.Health.Delivered)
	}
	utterances := a.VAD.Run(ctx, segmentPCM, a.Recorder.Format())
	if hasManaged {
		defer func(out <-chan vad.Utterance) {
			cancelCapture()
			deadline := time.NewTimer(captureStopTimeout)
			defer deadline.Stop()
			for {
				select {
				case _, open := <-out:
					if !open {
						managed.Close() // discard: Managed.Close has no return value.
						return
					}
				case <-deadline.C:
					// Never free native detector state while Accept may still
					// be using it. Service shutdown remains bounded.
					a.logf("wake detector did not stop within %s; leaving its native state intact", captureStopTimeout)
					return
				}
			}
		}(utterances)
	}
	var fired <-chan timer.Timer
	if a.Timers != nil {
		done := make(chan struct{})
		defer close(done)

		go a.Timers.Run(done)
		fired = a.Timers.Fired()
		a.announceMissed(ctx, a.Missed)
	}
	// Only Run owns speech and feedback. The worker owns the entire existing
	// HandleText path (including typed actions and proposals), never the player.
	type plannerResult struct {
		id      uint64
		plan    brain.Plan
		err     error
		started time.Time
	}
	type plannerWork struct {
		id                uint64
		ctx               context.Context
		cancel            context.CancelFunc
		done              chan struct{}
		stop              func()
		until             time.Time
		remaining         int
		cancelRequested   bool
		followUp          bool
		allowConversation bool
	}
	results := make(chan plannerResult, 1)
	var active *plannerWork
	var nextID uint64
	gate, hasGate := a.VAD.(conversationGate)
	windowEnabled := hasGate && a.InFlightTurns > 0 && a.InFlightWindow > 0
	conversationEnabled := hasGate && a.ConversationTurns > 0 && a.ConversationWindow > 0
	var conversationUntil time.Time
	var conversationRemaining int
	var conversationTimer *time.Timer
	var conversationExpiry <-chan time.Time
	closeConversation := func() {
		conversationUntil = time.Time{}
		conversationRemaining = 0
		if conversationTimer != nil {
			conversationTimer.Stop()
			conversationTimer = nil
		}
		conversationExpiry = nil
		if hasGate {
			gate.SetInFlightWindow(time.Time{}, 0)
		}
		if active != nil {
			active.until = time.Time{}
			active.remaining = 0
			active.allowConversation = false
		} else {
			a.Feedback.Restore()
		}
	}
	defer closeConversation()
	var captureFaults <-chan struct{}
	if reporter, ok := a.VAD.(interface{ Discontinuities() <-chan struct{} }); ok {
		captureFaults = reporter.Discontinuities()
	}
	captureInterrupted := func() bool {
		select {
		case <-captureFaults:
			a.logf("stage=conversation state=closed reason=capture_discontinuity")
			closeConversation()
			return true
		default:
			return false
		}
	}
	finishAnswer := func(followUp bool) {
		if captureInterrupted() || !conversationEnabled || ctx.Err() != nil {
			return
		}
		if followUp && (conversationRemaining <= 0 || !time.Now().Before(conversationUntil)) {
			closeConversation()
			return
		}
		// The ordinary earcon/light is the listening cue, never another spoken
		// instruction. Echo suppression covers both it and the hardware tail.
		if !a.acknowledge(ctx) || captureInterrupted() {
			closeConversation()
			return
		}
		if !followUp {
			conversationUntil = time.Now().Add(playbackTail + a.ConversationWindow)
			conversationRemaining = a.ConversationTurns
			conversationTimer = time.NewTimer(time.Until(conversationUntil))
			conversationExpiry = conversationTimer.C
		}
		if time.Now().Before(conversationUntil) {
			gate.SetInFlightWindow(conversationUntil, conversationRemaining)
			a.logf("stage=conversation state=open remaining=%d", conversationRemaining)
		} else {
			closeConversation()
		}
	}
	defer func() {
		if active == nil {
			return
		}
		active.cancel()
		closeConversation()
		active.stop()
		select {
		case <-active.done:
		case <-time.After(plannerStopTimeout):
			a.logf("planner did not stop within %s; exiting without its result", plannerStopTimeout)
		}
	}()
	// A timer or a busy acknowledgement can speak while a planner is pending.
	// Stop and join its sound first, then resume it if work is still pending.
	speakDuringWork := func(fn func() bool) {
		if hasGate {
			gate.SetInFlightWindow(time.Time{}, 0)
		}
		if active != nil {
			active.stop()
		}
		if !fn() {
			closeConversation()
		}
		captureInterrupted()
		if active != nil {
			select {
			case <-active.done:
				return // Completion during speech must not reopen capture.
			default:
			}
		}
		if active != nil && active.ctx.Err() == nil && ctx.Err() == nil {
			if active.remaining > 0 && time.Now().Before(active.until) {
				gate.SetInFlightWindow(active.until, active.remaining)
			}
			active.stop = a.Feedback.Thinking(ctx)
		} else if active == nil && conversationRemaining > 0 && time.Now().Before(conversationUntil) {
			gate.SetInFlightWindow(conversationUntil, conversationRemaining)
		}
	}
	accept := func(command string, stopThinking func(), acknowledge, followUp bool) (stop bool) {
		if stopThinking == nil {
			stopThinking = func() {}
		}
		stopThinking = sync.OnceFunc(stopThinking)
		if captureInterrupted() {
			stopThinking()
			return false
		}
		if conversationClose(command) {
			stopThinking()
			closeConversation()
			speakDuringWork(func() bool { return a.speakLogged(ctx, "Okay.") })
			return false
		}
		if active != nil {
			stopThinking()
			// OMP is single-flight. No busy turn can start another planner.
			select {
			case <-active.done:
				return false // The completed result is already queued.
			default:
			}
			speakDuringWork(func() bool {
				if acknowledge {
					a.acknowledge(ctx)
				}
				if handled, spoken := a.timerControl(ctx, command, func() {}); handled {
					return spoken
				}
				if reply, shutdown, handled := localControl(command); handled {
					if shutdown {
						active.cancelRequested = true
						active.cancel()
					}
					stop = shutdown
					return a.speakLogged(ctx, reply)
				}
				switch inFlightControl(command) {
				case "cancel":
					active.cancelRequested = true
					active.cancel()
					closeConversation()
					return a.speakLogged(ctx, "Cancellation requested. Work already sent may still finish.")
				case "status":
					if active.cancelRequested {
						return a.speakLogged(ctx, "This voice request is being cancelled. Work already started elsewhere may still be running.")
					}
					return a.speakLogged(ctx, "I'm still working on your request. I don't have a confirmed result yet.")
				case "correction":
					return a.speakLogged(ctx, "I can't change work already underway. Please ask again after it finishes.")
				default:
					if followUp {
						a.logf("stage=busy state=ignored reason=not_a_control")
						return true
					}
					reply := "I'm still working on your previous request."
					if a.Front != nil && time.Now().Before(active.until) && active.ctx.Err() == nil {
						if candidate, err := a.frontReply(active.ctx, command); err == nil {
							reply = candidate
						} else {
							a.logf("stage=front state=fallback err=%v", err)
						}
					}
					select {
					case <-active.done:
						return true // Do not narrate a stale state before the final result.
					default:
					}
					return a.speakLogged(ctx, reply)
				}
			})
			return stop
		}
		if command == "" {
			stopThinking()
			closeConversation()
			a.speakLogged(ctx, "Please say Hey Voice followed by your request.")
			return false
		}
		if handled, spoken := a.timerControl(ctx, command, stopThinking); handled {
			a.Feedback.Restore()
			if spoken {
				finishAnswer(followUp)
			} else {
				closeConversation()
			}
			return false
		}
		if control := inFlightControl(command); control == "cancel" || control == "status" {
			stopThinking()
			closeConversation()
			a.speakLogged(ctx, "There's no request running.")
			return false
		}
		if reply, shutdown, handled := localControl(command); handled {
			stopThinking()
			a.Feedback.Restore()
			if a.speakLogged(ctx, reply) && !shutdown {
				finishAnswer(followUp)
			} else {
				closeConversation()
			}
			return shutdown
		}
		nextID++
		workCtx, cancel := context.WithCancel(ctx)
		work := &plannerWork{id: nextID, ctx: workCtx, cancel: cancel, done: make(chan struct{}), stop: stopThinking, followUp: followUp, allowConversation: true}
		active = work
		go func() {
			defer close(work.done)
			started := time.Now()
			p, err := a.HandleText(workCtx, command)
			results <- plannerResult{id: work.id, plan: p, err: err, started: started}
		}()
		if windowEnabled || followUp {
			// Keep processing feedback running; no routine spoken preamble.
			select {
			case <-work.done:
				return false
			default:
			}
			if followUp {
				work.until, work.remaining = conversationUntil, conversationRemaining
			} else {
				work.until = time.Now().Add(a.InFlightWindow)
				work.remaining = a.InFlightTurns
			}
			if work.ctx.Err() == nil && work.remaining > 0 && time.Now().Before(work.until) {
				gate.SetInFlightWindow(work.until, work.remaining)
			}
		}
		return false
	}
	beginAuthorized := func(acknowledge bool) (func(), bool) {
		if active != nil {
			// Do not start a second thinking loop or play the wake sound over
			// current feedback. Busy feedback is serialized before speech.
			return func() {}, acknowledge
		}
		return a.beginProcessing(ctx, acknowledge), false
	}

	healthTick := time.NewTicker(time.Second)
	defer healthTick.Stop()
	for {
		a.Health.LoopProgress()
		if utterances == nil && active == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-healthTick.C:
			a.Health.LoopProgress()
		case <-captureFaults:
			a.logf("stage=conversation state=closed reason=capture_discontinuity")
			closeConversation()
		case err, ok := <-recErr:
			if ok && err != nil {
				a.Health.Failed("capture", "failed")
				a.logf("stage=capture state=failed err=%v", err)
				return err
			}
			recErr = nil
		case <-conversationExpiry:
			a.logf("stage=conversation state=closed reason=deadline")
			closeConversation()
		case t, ok := <-fired:
			if ctx.Err() != nil {
				continue
			}
			if !ok {
				fired = nil
				continue
			}
			speakDuringWork(func() bool { return a.announceFired(ctx, t) })
		case result := <-results:
			if active == nil || result.id != active.id {
				a.logf("stage=plan state=stale id=%d", result.id)
				continue
			}
			captureInterrupted()
			active.stop()
			cancelRequested := active.cancelRequested
			followUp := active.followUp
			allowConversation := active.allowConversation
			active.cancel()
			if hasGate {
				gate.SetInFlightWindow(time.Time{}, 0)
			}
			active = nil
			if ctx.Err() != nil {
				continue
			}
			a.Feedback.Restore()
			if cancelRequested {
				closeConversation()
				a.logf("stage=plan state=cancelled ms=%.3f err=%v", sinceMS(result.started), result.err)
				continue
			}
			if result.err != nil {
				closeConversation()
				a.logf("stage=plan state=failed ms=%.3f err=%v", sinceMS(result.started), result.err)
				a.say(ctx, "I couldn't do that.")
				continue
			}
			a.logf("stage=plan state=ok ms=%.3f reply=%d", sinceMS(result.started), len(result.plan.Speak))
			if a.speakLogged(ctx, result.plan.Speak) && strings.TrimSpace(result.plan.Speak) != "" && allowConversation {
				finishAnswer(followUp)
			} else {
				closeConversation()
			}
		case u, ok := <-utterances:
			if !ok {
				if ctx.Err() == nil {
					a.Health.WakeState("failed")
					a.Health.Failed("segment", "closed")
				}
				a.logf("stage=segment state=closed")
				utterances = nil
				closeConversation()
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			if hasGate && !gate.Accepts(u) {
				a.logf("stage=segment state=dropped reason=closed_gate")
				continue
			}
			wakeFree := u.Keyword == "IN_FLIGHT"
			var authorizedUntil time.Time
			if wakeFree {
				if active != nil {
					if active.remaining <= 0 || !time.Now().Before(active.until) || active.ctx.Err() != nil {
						continue
					}
					authorizedUntil = active.until
					active.remaining--
					if active.followUp {
						conversationRemaining--
					}
				} else {
					if !conversationEnabled || conversationRemaining <= 0 || !time.Now().Before(conversationUntil) {
						continue
					}
					authorizedUntil = conversationUntil
					conversationRemaining--
				}
			} else if active == nil {
				closeConversation() // Only a fresh local wake may start a new budget.
			}
			a.Health.Segment()
			ms := len(u.PCM) * 1000 / max(u.Format.SampleRate, 1)
			a.logf("stage=segment state=ok samples=%d ms=%d peak=%.4f truncated=%v",
				len(u.PCM), ms, u.Peak, u.Truncated)
			if u.WakeMatched {
				if !wakeFree {
					a.Health.WakeDetected()
				}
				// The streaming detector has already proved this utterance is
				// wake-authorized, so processing feedback can cover STT too.
				// Inline commands acknowledge here; standalone follow-ups
				// were acknowledged before their command capture began.
				stopThinking, acknowledge := beginAuthorized(!u.WakeAcknowledged)
				started := time.Now()
				text, err := a.transcribe(ctx, a.STT, "transcribe", u)
				if err != nil {
					stopThinking()
					closeConversation()
					if active == nil {
						a.Feedback.Restore()
					}
					a.logf("stage=transcribe state=failed ms=%.3f err=%v", sinceMS(started), err)
					continue
				}
				text = strings.TrimSpace(text)
				if text == "" {
					stopThinking()
					closeConversation()
					a.logf("stage=transcribe state=empty ms=%.3f peak=%.4f", sinceMS(started), u.Peak)
					continue
				}
				a.logf("stage=transcribe state=ok ms=%.3f heard=%q peak=%.4f", sinceMS(started), text, u.Peak)
				if wakeFree && !time.Now().Before(authorizedUntil) {
					stopThinking()
					closeConversation()
					continue
				}
				command := text
				if cloudMatched, cloudCommand, _ := wake.Match(text, a.WakePhrases, a.WakeFuzz); cloudMatched {
					command = cloudCommand
				}
				command = a.correctCommand(command)
				a.logf("stage=wake state=accepted source=streaming keyword=%q command=%q follow_up=%v", u.Keyword, command, u.WakeAcknowledged)
				if stop := accept(command, stopThinking, acknowledge, wakeFree); stop {
					return nil
				}
				continue
			}
			if a.WakeSTT != nil {
				started := time.Now()
				gateText, err := a.transcribe(ctx, a.WakeSTT, "wake_transcribe", u)
				if err != nil {
					a.Health.WakeState("failed")
					a.logf("stage=wake state=failed source=local-stt ms=%.3f err=%v", sinceMS(started), err)
					continue
				}
				a.Health.WakeState("ready")
				gateText = strings.TrimSpace(gateText)
				a.logf("stage=transcribe state=ok source=local-stt ms=%.3f heard=%q peak=%.4f", sinceMS(started), gateText, u.Peak)
				matched, command, _ := wake.Match(gateText, a.WakePhrases, a.WakeFuzz)
				if !matched {
					a.logf("stage=wake state=nomatch source=local-stt ms=%.3f", sinceMS(started))
					continue
				}
				a.Health.WakeDetected()
				a.logf("stage=wake state=matched source=local-stt ms=%.3f", sinceMS(started))
				// This local transcript is the privacy gate: feedback begins
				// only after it proves the utterance contains a wake phrase.
				stopThinking, acknowledge := beginAuthorized(true)
				if command == "" {
					a.logf("stage=wake state=accepted command=%q", command)
					if stop := accept(command, stopThinking, acknowledge, false); stop {
						return nil
					}
					continue
				}

				started = time.Now()
				text, err := a.transcribe(ctx, a.STT, "transcribe", u)
				if err != nil {
					stopThinking()
					if active == nil {
						a.Feedback.Restore()
					}
					a.logf("stage=transcribe state=failed ms=%.3f err=%v", sinceMS(started), err)
					continue
				}
				text = strings.TrimSpace(text)
				if text == "" {
					stopThinking()
					if active == nil {
						a.Feedback.Restore()
					}
					a.logf("stage=transcribe state=empty ms=%.3f peak=%.4f", sinceMS(started), u.Peak)
					continue
				}
				a.logf("stage=transcribe state=ok ms=%.3f heard=%q peak=%.4f", sinceMS(started), text, u.Peak)
				if cloudMatched, cloudCommand, _ := wake.Match(text, a.WakePhrases, a.WakeFuzz); cloudMatched {
					command = cloudCommand
				}
				command = a.correctCommand(command)
				a.logf("stage=wake state=accepted command=%q", command)
				if stop := accept(command, stopThinking, acknowledge, false); stop {
					return nil
				}
				continue
			}

			started := time.Now()
			text, err := a.transcribe(ctx, a.STT, "wake_transcribe", u)
			if err != nil {
				a.Health.WakeState("failed")
				a.logf("stage=transcribe state=failed ms=%.3f err=%v", sinceMS(started), err)
				continue
			}
			a.Health.WakeState("ready")
			text = strings.TrimSpace(text)
			if text == "" {
				a.logf("stage=transcribe state=empty ms=%.3f peak=%.4f", sinceMS(started), u.Peak)
				continue
			}
			a.logf("stage=transcribe state=ok ms=%.3f heard=%q peak=%.4f", sinceMS(started), text, u.Peak)
			matched, command, _ := wake.Match(text, a.WakePhrases, a.WakeFuzz)
			if !matched {
				a.logf("stage=wake state=nomatch")
				continue
			}
			a.Health.WakeDetected()
			command = a.correctCommand(command)
			a.logf("stage=wake state=matched command=%q", command)
			stopThinking, acknowledge := beginAuthorized(true)
			if stop := accept(command, stopThinking, acknowledge, false); stop {
				return nil
			}
		}
	}
}

// correctCommand never participates in wake matching. Keep the engine's raw
// heard= diagnostic above, and log both command forms only when a rule changes
// text. In a remote pipeline the raw local gate command remains the fallback
// when the remote transcript does not contain an authorized wake prefix.
func (a *Assistant) correctCommand(raw string) string {
	corrected := a.Vocabulary.Correct(raw)
	if corrected != raw {
		a.logf("stage=vocabulary state=corrected context=%q raw=%q corrected=%q", a.Vocabulary.Context(), raw, corrected)
	}
	return corrected
}

// captureStopTimeout bounds the wait for capture to unwind at shutdown. A
// stuck capture command must not keep the service from exiting; systemd's
// stop timeout would kill it anyway, and this way the delay is ours and
// explained rather than a hang.
const captureStopTimeout = 2 * time.Second

// A planner that ignores cancellation cannot hold shutdown indefinitely.
const plannerStopTimeout = 2 * time.Second

// awaitCaptureStop drains samples until the recorder closes the channel, which
// it does only after its deferred cleanup has run.
func (a *Assistant) awaitCaptureStop(pcm <-chan []int16) {
	if pcm == nil {
		return
	}
	deadline := time.NewTimer(captureStopTimeout)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-pcm:
			if !ok {
				return
			}
		case <-deadline.C:
			a.logf("capture did not stop within %s; exiting without its cleanup", captureStopTimeout)
			return
		}
	}
}

// acknowledge suppresses inline wake and conversation-cue echo. The keyword
// segmenter owns suppression for its standalone-wake acknowledgement.
func (a *Assistant) acknowledge(ctx context.Context) bool {
	if gate, ok := a.VAD.(conversationGate); ok {
		gate.Suspend()
		defer gate.ResumeAfter(playbackTail)
	}
	return a.Feedback.Accepted(ctx)
}

// beginProcessing starts all local feedback for one wake-authorized request.
// It is deliberately called before remote transcription whenever the local
// streaming detector or local STT gate has already established authorization.
func (a *Assistant) beginProcessing(ctx context.Context, acknowledge bool) func() {
	if acknowledge {
		a.acknowledge(ctx)
	}
	return a.Feedback.Thinking(ctx)
}

// speakLogged times synthesis and playback together, which is what a person
// waits through, and reports the text length so a slow reply can be told from
// a long one.
func (a *Assistant) speakLogged(ctx context.Context, text string) bool {
	started := time.Now()
	if err := a.Speak(ctx, text); err != nil {
		a.logf("stage=speak state=failed ms=%.3f chars=%d err=%v", sinceMS(started), len(text), err)
		return false
	}
	a.logf("stage=speak state=ok ms=%.3f chars=%d", sinceMS(started), len(text))
	return true
}

// sinceMS reports elapsed milliseconds with fractions. Milliseconds() alone
// truncates toward zero, so a sub-millisecond stage logged ms=0, which is
// indistinguishable at a glance from a stage that was never measured - the
// exact ambiguity this logging exists to remove.
func sinceMS(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

func localControl(command string) (reply string, stop bool, handled bool) {
	switch strings.Trim(strings.ToLower(strings.TrimSpace(command)), " .!?") {
	case "turn off", "turn yourself off", "stop listening", "go offline":
		return "Turning off.", true, true
	case "turn on", "start listening", "go online":
		return "I'm already on.", false, true
	default:
		return "", false, false
	}
}

func conversationClose(command string) bool {
	switch strings.Trim(strings.ToLower(strings.TrimSpace(command)), " .!?") {
	case "that's all", "that s all", "that is all", "no thanks", "no thank you", "goodbye", "end conversation":
		return true
	}
	return false
}

// inFlightControl handles only task controls. Named timer/device requests keep
// their existing deterministic routes; bare stop/cancel closes conversation.
func inFlightControl(command string) string {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(command)), " .!?")
	switch normalized {
	case "stop", "cancel", "cancel that", "cancel the request", "stop working on that", "stop that request":
		return "cancel"
	case "status", "what is the status", "what's the status", "what s the status", "what's happening", "what s happening", "what is happening", "are you done", "how's it going", "how s it going":
		return "status"
	}
	if strings.HasPrefix(normalized, "actually ") || strings.HasPrefix(normalized, "make that ") ||
		strings.HasPrefix(normalized, "instead ") {
		return "correction"
	}
	return ""
}
