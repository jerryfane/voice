package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/health"
	"github.com/jerryfane/voice/internal/vad"
)

type deadlineTranscriber struct {
	monitor *health.Monitor
	t       *testing.T
}

func (s deadlineTranscriber) Transcribe(ctx context.Context, _ []int16, _ audio.Format) (string, error) {
	snapshot := s.monitor.Snapshot(time.Now())
	if len(snapshot.Stages) != 1 || snapshot.Stages[0].Name != "transcribe" {
		s.t.Fatalf("stage not exposed during work: %+v", snapshot.Stages)
	}
	if _, ok := ctx.Deadline(); !ok {
		s.t.Fatal("unbounded transcriber")
	}
	<-ctx.Done()
	// A late successful provider response must not authorize a command.
	return "turn off", nil
}
func (deadlineTranscriber) Name() string              { return "deadline-test" }
func (deadlineTranscriber) Available() (bool, string) { return true, "test" }

func TestTranscriptionBudgetRejectsLateResultAndRecordsFailure(t *testing.T) {
	m := health.New()
	a := &Assistant{Health: m, HealthBudgets: HealthBudgets{Transcribe: time.Millisecond}}
	_, err := a.transcribe(context.Background(), deadlineTranscriber{monitor: m, t: t}, "transcribe", vad.Utterance{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late success accepted: %v", err)
	}
	s := m.Snapshot(time.Now())
	if len(s.Stages) != 0 || s.LastFailure.Stage != "transcribe" || s.LastFailure.Kind != "deadline" {
		t.Fatalf("completion: %+v", s)
	}
}

func TestConfiguredPlanBudgetIsNotShortened(t *testing.T) {
	b := HealthBudgets{Plan: 20 * time.Minute}
	if b.plan() != 20*time.Minute {
		t.Fatal("configured provider budget shortened")
	}
}
