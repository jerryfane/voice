// Package health exposes private, content-free observations of the live listener.
// Fresh PCM proves transport activity, not a working microphone or wake model.
package health

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	FreshFor        = 5 * time.Second
	PublishEvery    = time.Second
	CaptureFreshFor = 3 * time.Second
)

type Stage struct {
	Name     string    `json:"name"`
	Started  time.Time `json:"started"`
	Deadline time.Time `json:"deadline"`
	AgeMS    int64     `json:"age_ms"`
	Overdue  bool      `json:"overdue"`
}

type Failure struct {
	Stage string    `json:"stage"`
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
}

type Snapshot struct {
	Version          int       `json:"version"`
	PID              int       `json:"pid"`
	BootID           string    `json:"boot_id"`
	ProcessStart     string    `json:"process_start"`
	Started          time.Time `json:"started"`
	Updated          time.Time `json:"updated"`
	CaptureState     string    `json:"capture_state"`
	LastFrame        time.Time `json:"last_frame"`
	LastDelivered    time.Time `json:"last_delivered"`
	LastSignal       time.Time `json:"last_nonzero_sample"`
	Frames           uint64    `json:"frames"`
	Peak             int64     `json:"latest_frame_peak"`
	Discontinuities  uint64    `json:"discontinuities"`
	WakeState        string    `json:"wake_state"`
	LastWakeProgress time.Time `json:"last_wake_progress"`
	WakeGate         string    `json:"wake_gate"`
	LastWake         time.Time `json:"last_wake"`
	LastSegment      time.Time `json:"last_segment"`
	LastLoop         time.Time `json:"last_event_loop_progress"`
	Problem          bool      `json:"problem"`
	Stages           []Stage   `json:"stages"`
	LastFailure      Failure   `json:"last_failure"`
	Diagnosis        string    `json:"diagnosis"`
}

// Monitor stores only scalar audio observations, never samples or transcripts.
// Frame hooks do no I/O and allocate nothing. Stage updates are off the hot path.
type Monitor struct {
	started   time.Time
	frame     atomic.Int64
	delivered atomic.Int64
	signal    atomic.Int64
	frames    atomic.Uint64
	peak      atomic.Int64
	breaks    atomic.Uint64
	progress  atomic.Int64
	wake      atomic.Int64
	segment   atomic.Int64
	loop      atomic.Int64
	mu        sync.Mutex
	wakeState string
	wakeGate  func() string
	failure   Failure
	next      uint64
	stages    map[uint64]Stage
}

func New() *Monitor {
	return &Monitor{started: time.Now(), wakeState: "starting", stages: make(map[uint64]Stage)}
}

func (m *Monitor) Capture(pcm []int16) {
	if m == nil {
		return
	}
	if pcm == nil {
		m.breaks.Add(1)
		return
	}
	if len(pcm) == 0 {
		return
	}
	var peak int64
	for _, sample := range pcm {
		v := int64(sample)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	now := time.Now().UnixNano()
	m.peak.Store(peak)
	if peak != 0 {
		m.signal.Store(now)
	}
	m.frames.Add(1)
	m.frame.Store(now)
}

func (m *Monitor) Delivered() {
	if m != nil {
		m.delivered.Store(time.Now().UnixNano())
	}
}
func (m *Monitor) WakeProgress() {
	if m != nil {
		m.progress.Store(time.Now().UnixNano())
	}
}
func (m *Monitor) WakeDetected() {
	if m != nil {
		m.wake.Store(time.Now().UnixNano())
	}
}
func (m *Monitor) Segment() {
	if m != nil {
		m.segment.Store(time.Now().UnixNano())
	}
}
func (m *Monitor) LoopProgress() {
	if m != nil {
		m.loop.Store(time.Now().UnixNano())
	}
}
func (m *Monitor) WakeState(state string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.wakeState = state
	m.mu.Unlock()
}

// SetWakeGate installs a content-free, concurrency-safe gate observer.
func (m *Monitor) SetWakeGate(observe func() string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.wakeGate = observe
	m.mu.Unlock()
}
func (m *Monitor) Failed(stage, kind string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.failure = Failure{Stage: stage, Kind: kind, At: time.Now()}
	m.mu.Unlock()
}

// Begin supplies a total stage budget in addition to provider-specific deadlines.
// A provider ignoring cancellation stays visible as overdue; no orphaned retry
// goroutine or restart watchdog hides that failure.
func (m *Monitor) Begin(ctx context.Context, name string, budget time.Duration) (context.Context, func(error)) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	var id uint64
	if m != nil {
		deadline, _ := ctx.Deadline()
		m.mu.Lock()
		m.next++
		id = m.next
		m.stages[id] = Stage{Name: name, Started: time.Now(), Deadline: deadline}
		m.mu.Unlock()
	}
	return ctx, func(err error) {
		if err == nil {
			err = ctx.Err()
		}
		if m != nil {
			m.mu.Lock()
			delete(m.stages, id)
			if err != nil {
				kind := "failed"
				if ctx.Err() == context.DeadlineExceeded {
					kind = "deadline"
				} else if ctx.Err() == context.Canceled {
					kind = "cancelled"
				}
				m.failure = Failure{Stage: name, Kind: kind, At: time.Now()}
			}
			m.mu.Unlock()
		}
		cancel()
	}
}

func stamp(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (m *Monitor) Snapshot(now time.Time) Snapshot {
	s := Snapshot{Version: 1, Started: m.started, Updated: now,
		LastFrame: stamp(m.frame.Load()), LastDelivered: stamp(m.delivered.Load()),
		LastSignal: stamp(m.signal.Load()), Frames: m.frames.Load(), Peak: m.peak.Load(),
		Discontinuities: m.breaks.Load(), LastWakeProgress: stamp(m.progress.Load()),
		LastWake: stamp(m.wake.Load()), LastSegment: stamp(m.segment.Load()), LastLoop: stamp(m.loop.Load())}
	m.mu.Lock()
	s.WakeState, s.LastFailure = m.wakeState, m.failure
	gate := m.wakeGate
	// Preserve stage start order even when work overlaps with spoken status.
	for _, stage := range m.stages {
		stage.AgeMS = max(0, now.Sub(stage.Started).Milliseconds())
		stage.Overdue = now.After(stage.Deadline)
		s.Stages = append(s.Stages, stage)
	}
	m.mu.Unlock()
	if gate != nil {
		s.WakeGate = gate()
	}
	sort.Slice(s.Stages, func(i, j int) bool { return s.Stages[i].Started.Before(s.Stages[j].Started) })
	s.CaptureState = "flowing"
	s.Diagnosis = "capture flowing; wake recognition not proven by stream activity"
	if s.LastWake.IsZero() {
		s.Diagnosis = "capture flowing; no wake observed (not proof of failure)"
	}
	if s.LastSignal.IsZero() || now.Sub(s.LastSignal) > CaptureFreshFor {
		s.CaptureState = "quiet"
		s.Diagnosis = "fresh digital silence; quiet room, mute or input routing cannot be distinguished"
	}
	if s.LastFrame.IsZero() || now.Sub(s.LastFrame) > CaptureFreshFor {
		s.CaptureState = "stalled"
		s.Diagnosis = "no fresh PCM delivered by recorder; capture transport stalled or recovering"
		s.Problem = true
		if s.LastFrame.IsZero() && now.Sub(s.Started) <= CaptureFreshFor {
			s.CaptureState = "starting"
			s.Diagnosis = "waiting for first capture frame"
			s.Problem = false
		}
	} else if s.LastDelivered.IsZero() || now.Sub(s.LastDelivered) > CaptureFreshFor {
		s.Diagnosis = "capture received but segmentation is not consuming frames"
		s.Problem = true
	} else if s.WakeState == "ready" && !s.LastWakeProgress.IsZero() && now.Sub(s.LastWakeProgress) > CaptureFreshFor {
		s.Diagnosis = "wake processing progress is stale despite fresh capture"
		s.Problem = true
	}
	if s.LastFrame.After(s.LastDelivered) && now.Sub(s.LastFrame) > CaptureFreshFor {
		s.Diagnosis = "segmentation is not consuming a pending frame; upstream capture liveness unverified"
		s.Problem = true
	}
	if !s.LastLoop.IsZero() && now.Sub(s.LastLoop) > FreshFor && len(s.Stages) == 0 {
		s.Diagnosis = "listener event loop has stopped progressing"
		s.Problem = true
	}
	if s.WakeState == "failed" {
		s.Diagnosis = "wake detector failed; capture activity does not prove readiness"
		s.Problem = true
	}
	for _, stage := range s.Stages {
		if stage.Name == "wake_start" && !stage.Overdue && s.LastFrame.IsZero() {
			s.CaptureState = "starting"
			s.Diagnosis = "initializing wake detector before capture starts"
			s.Problem = false
		}
		if stage.Overdue {
			s.Diagnosis = "stage exceeded its deadline: " + stage.Name
			s.Problem = true
			break
		}
	}
	return s
}
