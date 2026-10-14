package speech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
)

func TestSplitDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, text, direction, spoken string
	}{
		{"plain", "  Hello [world].  ", "", "  Hello [world].  "},
		{"ordinary leading brackets", "[angry] Hello.", "", "[angry] Hello."},
		{"inline reserved marker", "Say [voice: angry] literally.", "", "Say [voice: angry] literally."},
		{"direction", " \t[voice: angry, but controlled]  Hello [world].\n", "angry, but controlled", "Hello [world]."},
		{"whisper", "[voice: whisper softly]Hello.", "whisper softly", "Hello."},
		{"directive only", "[voice: excited] \n", "excited", ""},
		{"unclosed", "[voice: angry Hello.", "", "[voice: angry Hello."},
		{"empty", "[voice:] Hello.", "", "[voice:] Hello."},
		{"blank", "[voice: \t] Hello.", "", "[voice: \t] Hello."},
		{"newline", "[voice: angry\nquiet] Hello.", "", "[voice: angry\nquiet] Hello."},
		{"trailing newline", "[voice: angry\n] Hello.", "", "[voice: angry\n] Hello."},
		{"carriage return", "[voice: angry\rquiet] Hello.", "", "[voice: angry\rquiet] Hello."},
		{"unicode newline", "[voice: angry\u2028quiet] Hello.", "", "[voice: angry\u2028quiet] Hello."},
		{"nested bracket", "[voice: [angry]] Hello.", "", "[voice: [angry]] Hello."},
		{"wrong prefix", "[Voice: angry] Hello.", "", "[Voice: angry] Hello."},
		{"preserve inline metadata", "[voice: calm] Say [voice: angry] literally.", "calm", "Say [voice: angry] literally."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			direction, spoken := SplitDelivery(tc.text)
			if direction != tc.direction || spoken != tc.spoken {
				t.Fatalf("SplitDelivery(%q) = (%q, %q), want (%q, %q)", tc.text, direction, spoken, tc.direction, tc.spoken)
			}
		})
	}
}

func TestNativeSpeechDeliveryModelBoundary(t *testing.T) {
	for _, tc := range []struct {
		model, text, want string
	}{
		{"fish-audio/drama-3", "[voice: angry, speaking firmly] Hello [world].", "[angry, speaking firmly] Hello [world]."},
		{"fish-audio/drama-3-20261013", "[voice: excited] Hello.", "[excited] Hello."},
		{"fish-audio/drama-3-other", "[voice: excited] Hello.", "Hello."},
		{"fish-audio/drama-3-20269999", "[voice: excited] Hello.", "Hello."},
		{"fish-audio/drama-30", "[voice: excited] Hello.", "Hello."},
		{"other/drama-3", "[voice: excited] Hello.", "Hello."},
		{"openai/tts-1", "[voice: whisper] Hello [world].", "Hello [world]."},
		{"fish-audio/drama-3", "[ordinary] Hello [world].", "[ordinary] Hello [world]."},
	} {
		t.Run(tc.model+"/"+tc.text, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Input != tc.want {
					t.Errorf("engine input = %q, want %q", request.Input, tc.want)
				}
				w.Header().Set("Content-Type", "audio/pcm;rate=24000;channels=1")
				if _, err := w.Write([]byte{1, 0}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			synth := NewOpenRouterSynthesizer(server.URL+"/audio/speech", tc.model, "", "secret", time.Second, nil)
			synth.Client = server.Client()
			if _, err := synth.Synthesize(context.Background(), tc.text); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConversationalSpeechStripsDeliveryAndVerifiesPlainTranscript(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []speechMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(request.Messages) != 2 || strings.Contains(request.Messages[1].Content, "[voice:") || !strings.Contains(request.Messages[1].Content, "Hello [world].") {
			t.Errorf("messages = %+v", request.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":%q,\"transcript\":\"Hello [world].\"}}}]}\n\n", base64.StdEncoding.EncodeToString([]byte{1, 0}))
		writeSSE(t, w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	// Even a Drama model must not receive native markup over the chat API.
	synth := NewOpenRouterSynthesizer(server.URL, "fish-audio/drama-3", "voice", "secret", time.Second, nil)
	synth.Client = server.Client()
	if _, err := synth.Synthesize(context.Background(), "[voice: whisper softly] Hello [world]."); err != nil {
		t.Fatal(err)
	}
}

func deliveryCommand(t *testing.T, textArg bool) (*CommandSynthesizer, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "spoken.txt")
	wave := filepath.Join(dir, "speech.wav")
	wav := audio.EncodeWAV([]int16{9}, audio.Default())
	if err := os.WriteFile(wave, wav, 0600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"sh", "-c", `cat > "$1"; cat "$2"`, "tts", capture, wave}
	if textArg {
		argv = []string{"sh", "-c", `printf '%s' "$3" > "$1"; cat "$2"`, "tts", capture, wave, "{text}"}
	}
	return NewCommandSynthesizer("piper", argv, "", time.Second, nil), capture, wav
}

func TestCommandSpeechStripsOnlyReservedDelivery(t *testing.T) {
	for _, textArg := range []bool{false, true} {
		for _, text := range []string{"[voice: angry] Hello [world].", "[ordinary] Hello [world].", "Say [voice: angry] literally.", "[voice: unfinished"} {
			local, capture, expectedWAV := deliveryCommand(t, textArg)
			wav, err := local.Synthesize(context.Background(), text)
			if err != nil {
				t.Fatal(err)
			}
			spoken, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			want := text
			if text == "[voice: angry] Hello [world]." {
				want = "Hello [world]."
			}
			if !textArg {
				want += "\n"
			}
			if string(spoken) != want || !bytes.Equal(wav, expectedWAV) {
				t.Fatalf("textArg=%v input=%q spoken=%q, want %q; WAV preserved=%v", textArg, text, spoken, want, bytes.Equal(wav, expectedWAV))
			}
		}
	}
}

func TestDeliveryFallbackDoesNotSpeakDirective(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	remote := NewOpenRouterSynthesizer(server.URL+"/audio/speech", "fish-audio/drama-3", "", "secret", time.Second, nil)
	remote.Client = server.Client()
	local, capture, expectedWAV := deliveryCommand(t, false)
	wav, err := NewSynthesizerCascade(nil, remote, local).Synthesize(context.Background(), "[voice: angry] Verified [receipt].")
	if err != nil {
		t.Fatal(err)
	}
	spoken, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(spoken) != "Verified [receipt].\n" || !bytes.Equal(wav, expectedWAV) {
		t.Fatalf("fallback spoken=%q; WAV preserved=%v", spoken, bytes.Equal(wav, expectedWAV))
	}
}

func TestDirectiveOnlySpeechRejectedBeforeExecution(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("directive-only request reached remote synthesis")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	remote := NewOpenRouterSynthesizer(server.URL+"/audio/speech", "fish-audio/drama-3", "", "secret", time.Second, nil)
	remote.Client = server.Client()
	local, capture, _ := deliveryCommand(t, false)
	for _, synth := range []Synthesizer{remote, local, NewSynthesizerCascade(nil, remote, local)} {
		if _, err := synth.Synthesize(context.Background(), "[voice: excited] \n"); err == nil || !strings.Contains(err.Error(), "speech text is empty") {
			t.Fatalf("%s directive-only error = %v", synth.Name(), err)
		}
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("directive-only request executed command: stat error = %v", err)
	}
}
