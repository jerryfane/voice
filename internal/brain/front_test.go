package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func frontCompletion(kind string) string {
	content := fmt.Sprintf(`{"kind":%q}`, kind)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":22,"completion_tokens":8,"cost":0.00002}}`, content)
}

func frontServer(t *testing.T, status int, body string, inspect func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inspect != nil {
			inspect(r)
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
}

func TestFrontReturnsOnlyLocalPhrasesAndVerifiedStatus(t *testing.T) {
	var logBuf bytes.Buffer
	server := frontServer(t, http.StatusOK, frontCompletion("status"), func(r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("method=%s authorization=%q", r.Method, r.Header.Get("Authorization"))
		}
		var sent struct {
			Model          string                           `json:"model"`
			Messages       []struct{ Role, Content string } `json:"messages"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
			Tools      json.RawMessage `json:"tools"`
			ToolChoice string          `json:"tool_choice"`
			MaxTokens  int             `json:"max_tokens"`
			Stream     bool            `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
			return
		}
		if sent.Model != "cheap-text" || len(sent.Messages) != 2 || sent.Messages[0].Role != "system" ||
			sent.Messages[1].Role != "user" || sent.ResponseFormat.Type != "json_object" ||
			len(sent.Tools) != 0 || sent.ToolChoice != "none" || sent.MaxTokens > 64 || sent.Stream {
			t.Errorf("unsafe front request: %+v", sent)
		}
		if !strings.Contains(sent.Messages[1].Content, "verified_status: Timer has 2 minutes left") {
			t.Errorf("status missing from request: %q", sent.Messages[1].Content)
		}
	})
	defer server.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	front := NewOpenRouterFrontDesk(server.URL, "cheap-text", "secret", time.Second, log.New(&logBuf, "", 0))
	front.client = server.Client()
	got, err := front.Reply(context.Background(), "How is my timer?", "Timer has 2 minutes left")
	if err != nil || got != "What I can verify: Timer has 2 minutes left" {
		t.Fatalf("Reply = %q, %v", got, err)
	}
	if text := logBuf.String(); !strings.Contains(text, "prompt_tokens=22") || !strings.Contains(text, "cost=2e-05") || strings.Contains(text, "Timer has") || strings.Contains(text, "secret") {
		t.Fatalf("unsafe/incomplete front log: %q", text)
	}
}

func TestFrontClarificationUsesBoundedQuestion(t *testing.T) {
	content := `{"kind":"clarify","question":"Did you mean the reminder for tomorrow?"}`
	body := fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, content)
	server := frontServer(t, http.StatusOK, body, nil)
	defer server.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	front := NewOpenRouterFrontDesk(server.URL, "cheap-text", "secret", time.Second, nil)
	front.client = server.Client()
	got, err := front.Reply(context.Background(), "Actually tomorrow", "")
	if err != nil || got != "Did you mean the reminder for tomorrow?" {
		t.Fatalf("clarification = %q, %v", got, err)
	}
}

func TestFrontRejectsProviderErrorsAndMalformedChoices(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP failure", http.StatusTooManyRequests, `{"error":{"message":"internal details"}}`},
		{"bad JSON", http.StatusOK, `{"choices":`},
		{"no choice", http.StatusOK, `{"choices":[]}`},
		{"incomplete", http.StatusOK, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"{\"kind\":\"status\"}"}}]}`},
		{"tool call", http.StatusOK, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"kind\":\"status\"}","tool_calls":[{"id":"bad"}]}}]}`},
		{"invented prose", http.StatusOK, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"kind\":\"status\",\"speak\":\"I turned off your lights\"}"}}]}`},
		{"unknown kind", http.StatusOK, frontCompletion("authorize_device")},
		{"unverified status", http.StatusOK, frontCompletion("status")},
		{"statement disguised as clarification", http.StatusOK, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"kind\":\"clarify\",\"question\":\"I completed your task.\"}"}}]}`},
		{"oversized", http.StatusOK, strings.Repeat("x", maxFrontResponse+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := frontServer(t, tc.status, tc.body, nil)
			defer server.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
			front := NewOpenRouterFrontDesk(server.URL, "cheap-text", "secret", time.Second, nil)
			front.client = server.Client()
			got, err := front.Reply(context.Background(), "Turn off the lights", "")
			if err == nil || got != "" {
				t.Fatalf("unsafe reply = %q, err = %v", got, err)
			}
			if strings.Contains(err.Error(), "internal details") || strings.Contains(err.Error(), "I turned off") {
				t.Fatalf("provider text leaked in error: %v", err)
			}
		})
	}
}

func TestFrontDoesNotFollowCredentialBearingRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	front := NewOpenRouterFrontDesk(source.URL, "cheap-text", "secret", time.Second, nil)
	// Trust only the test certificate; retain the constructor's redirect policy.
	front.client.(*http.Client).Transport = source.Client().Transport
	if got, err := front.Reply(context.Background(), "hello", ""); got != "" || err == nil {
		t.Fatalf("redirect unexpectedly produced speech: %q, %v", got, err)
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("provider redirected credential-bearing request %d times", got)
	}
}

func TestFrontRefusesOversizedInputsWithoutSending(t *testing.T) {
	var requests atomic.Int32
	server := frontServer(t, http.StatusOK, frontCompletion("unknown"), func(*http.Request) { requests.Add(1) })
	defer server.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	front := NewOpenRouterFrontDesk(server.URL, "cheap-text", "secret", time.Second, nil)
	front.client = server.Client()
	for _, input := range []struct{ utterance, status string }{
		{strings.Repeat("a", maxFrontUtterance+1), ""},
		{"hello", strings.Repeat("s", maxFrontStatus+1)},
	} {
		if got, err := front.Reply(context.Background(), input.utterance, input.status); got != "" || err == nil {
			t.Fatalf("oversized input returned %q, %v", got, err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("oversized inputs caused %d requests", got)
	}
}

func TestFrontRejectsLateHTTPResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // discard: http.Flusher.Flush has no result; the client deadline is the assertion.
		}
		<-r.Context().Done()
	}))
	defer server.Close() // discard: httptest.Server.Close only releases its test listener and has no result.
	front := NewOpenRouterFrontDesk(server.URL, "cheap-text", "secret", 20*time.Millisecond, nil)
	front.client = server.Client()
	if got, err := front.Reply(context.Background(), "hello", ""); got != "" || err == nil {
		t.Fatalf("late reply = %q, %v", got, err)
	}
}
