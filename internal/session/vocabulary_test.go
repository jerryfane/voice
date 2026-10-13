package session

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/brain"
	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/speech"
)

type vocabularyPlanner struct {
	countingPlanner
	command string
}

func (p *vocabularyPlanner) Plan(_ context.Context, command string, _ []device.Info) (brain.Plan, error) {
	p.calls++
	p.command = command
	return brain.Plan{Speak: "Okay."}, nil
}

func sessionVocabulary(t *testing.T) *speech.Vocabulary {
	t.Helper()
	c := &speech.VocabularyConfig{VocabularyScope: speech.VocabularyScope{
		Terms:        []string{"Björk", "Sunroom", "hey voice"},
		Replacements: []speech.Replacement{{From: "byork", To: "Björk"}, {From: "sun room", To: "Sunroom"}, {From: "hello device", To: "hey voice"}},
	}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return speech.NewVocabulary(c)
}

func TestVocabularySessionPreservesRawAndCorrectsAuthorizedCommands(t *testing.T) {
	for _, mode := range []string{"local", "streaming", "remote", "remote fallback"} {
		t.Run(mode, func(t *testing.T) {
			planner := &vocabularyPlanner{}
			raw := "hey voice do not play 2 byork songs in sun room"
			a, _, _ := feedbackAssistant(t, planner, raw)
			a.Vocabulary = sessionVocabulary(t)
			var journal strings.Builder
			a.Logger = log.New(&journal, "", 0)
			if mode == "streaming" {
				a.VAD = wakeMatchedSegmenter{}
			}
			if mode == "remote" || mode == "remote fallback" {
				a.WakeSTT = &queuedTranscriber{texts: []string{raw}}
			}
			if mode == "remote fallback" {
				a.STT = &queuedTranscriber{texts: []string{"unrelated cloud result"}}
			}
			if err := a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if planner.calls != 1 || planner.command != "do not play 2 Björk songs in Sunroom" {
				t.Fatalf("calls=%d command=%q", planner.calls, planner.command)
			}
			for _, want := range []string{
				`heard="hey voice do not play 2 byork songs in sun room"`,
				`raw="do not play 2 byork songs in sun room"`,
				`corrected="do not play 2 Björk songs in Sunroom"`,
			} {
				if !strings.Contains(journal.String(), want) {
					t.Fatalf("journal missing %q: %s", want, journal.String())
				}
			}
		})
	}
}

func TestVocabularyNeverAuthorizesWakeOrRemoteUpload(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			planner := &vocabularyPlanner{}
			a, _, _ := feedbackAssistant(t, planner, "hello device play byork")
			a.Vocabulary = sessionVocabulary(t)
			cloud := &countingTranscriber{texts: []string{"hey voice play byork"}}
			if remote {
				a.WakeSTT = a.STT
				a.STT = cloud
			}
			if err := a.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if planner.calls != 0 || cloud.calls != 0 {
				t.Fatalf("correction authorized speech: planner=%d cloud=%d", planner.calls, cloud.calls)
			}
		})
	}
}

func TestVocabularyEmptySessionKeepsOriginalCommand(t *testing.T) {
	planner := &vocabularyPlanner{}
	a, _, _ := feedbackAssistant(t, planner, "hey voice do not play 2 byork songs")
	a.Vocabulary = speech.NewVocabulary(&speech.VocabularyConfig{})
	var journal strings.Builder
	a.Logger = log.New(&journal, "", 0)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.command != "do not play 2 byork songs" || strings.Contains(journal.String(), "stage=vocabulary") {
		t.Fatalf("command=%q journal=%s", planner.command, journal.String())
	}
}

// Exercise the actual configured Build -> resident multipart -> raw wake gate
// -> correction -> planner path; only audio hardware and the HTTP server are
// fixtures. This must not require a model download or a cloud credential.
func TestConfiguredVocabularyResidentSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if got := r.FormValue("prompt"); got != "Sunroom, Björk" {
			t.Errorf("configured vocabulary prompt=%q", got)
		}
		if _, err := w.Write([]byte(`{"text":"hey voice do not play 2 byork songs in sun room"}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	c := config.Default()
	c.Input.TelephonyHID = ""
	c.Feedback.Light = "none"
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.Detector = "stt"
	c.STT.OpenRouter = nil
	c.STT.Resident = &config.ResidentSTT{Endpoint: server.URL, Timeout: config.Duration(time.Second)}
	c.STT.Vocabulary = &speech.VocabularyConfig{
		VocabularyScope: speech.VocabularyScope{Terms: []string{"Sunroom"}, Replacements: []speech.Replacement{{From: "sun room", To: "Sunroom"}}},
		Context:         "music",
		Contexts: map[string]speech.VocabularyScope{
			"music": {Terms: []string{"Björk"}, Replacements: []speech.Replacement{{From: "byork", To: "Björk"}}},
			"work":  {Terms: []string{"Acme"}},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	built := Build(c)
	cascade, ok := built.STT.(*speech.Cascade)
	if !ok || len(cascade.Engines) != 2 {
		t.Fatalf("configured local fallback missing: %T", built.STT)
	}
	command, ok := cascade.Engines[1].(*speech.CommandTranscriber)
	if !ok || command.Vocabulary != "Sunroom, Björk" {
		t.Fatalf("command fallback did not receive selected vocabulary: %+v", cascade.Engines[1])
	}
	planner := &vocabularyPlanner{}
	a, _, _ := feedbackAssistant(t, planner, "unused")
	a.STT = built.STT
	a.Vocabulary = built.Vocabulary
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if planner.command != "do not play 2 Björk songs in Sunroom" {
		t.Fatalf("configured path delivered %q", planner.command)
	}
}
