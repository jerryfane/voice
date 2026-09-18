package speech

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

func TestOpenRouterTranscriberSendsAcceptedAudioAndReadsCostedTranscript(t *testing.T) {
	var sawAuth, sawModel bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") == "Bearer secret"
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		sawModel = r.FormValue("model") == "openai/whisper-large-v3-turbo"
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
		} else if err := file.Close(); err != nil {
			t.Error(err)
		}
		if _, err := w.Write([]byte(`{"text":"Hey Voice, pause the music.","usage":{"seconds":2.1,"cost":0.00001}}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	tr := NewOpenRouterTranscriber(server.URL, "openai/whisper-large-v3-turbo", "secret", time.Second, nil)
	text, err := tr.Transcribe(context.Background(), []int16{1, 2, 3}, audio.Default())
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hey Voice, pause the music." {
		t.Fatalf("transcript = %q", text)
	}
	if !sawAuth || !sawModel {
		t.Fatalf("request auth=%v model=%v", sawAuth, sawModel)
	}
}

func TestOpenRouterMissingKeyFailsBeforeNetwork(t *testing.T) {
	tr := NewOpenRouterTranscriber("http://127.0.0.1:1", "model", "", time.Second, nil)
	_, err := tr.Transcribe(context.Background(), nil, audio.Default())
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("error = %v, want missing API key", err)
	}
}

func TestCascadeUsesLocalFallbackOnProviderFailure(t *testing.T) {
	primary := &stubTranscriber{name: "cloud", err: errors.New("provider unavailable")}
	local := &stubTranscriber{name: "local", text: "hey voice pause music"}
	cascade := NewCascade(nil, primary, local)
	text, err := cascade.Transcribe(context.Background(), nil, audio.Default())
	if err != nil {
		t.Fatal(err)
	}
	if text != local.text || primary.calls != 1 || local.calls != 1 {
		t.Fatalf("text=%q cloud calls=%d local calls=%d", text, primary.calls, local.calls)
	}
}

type stubTranscriber struct {
	name  string
	text  string
	err   error
	calls int
}

func (s *stubTranscriber) Transcribe(context.Context, []int16, audio.Format) (string, error) {
	s.calls++
	return s.text, s.err
}
func (s *stubTranscriber) Name() string              { return s.name }
func (s *stubTranscriber) Available() (bool, string) { return true, "available" }
