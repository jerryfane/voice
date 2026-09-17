package wake

import "testing"

func TestMatchExtractsInlineCommand(t *testing.T) {
	ok, cmd, phrase := Match("Hey, VOICE! turn the bedroom light purple", []string{"hey voice"}, 0.2)
	if !ok || cmd != "turn the bedroom light purple" || phrase != "hey voice" {
		t.Fatalf("got ok=%v cmd=%q phrase=%q", ok, cmd, phrase)
	}
}
func TestMatchToleratesSTTSpelling(t *testing.T) {
	ok, _, _ := Match("hey voise what time is it", []string{"hey voice"}, 0.25)
	if !ok {
		t.Fatal("expected fuzzy match")
	}
}
func TestNoMatchInsideUnrelatedSpeech(t *testing.T) {
	ok, _, _ := Match("the speaker was quiet", []string{"hey voice"}, 0.2)
	if ok {
		t.Fatal("unexpected wake match")
	}
}

func TestNoMatchWhenWakePhraseIsEmbeddedInConversation(t *testing.T) {
	ok, _, _ := Match("I heard someone say hey voice turn on the light", []string{"hey voice"}, 0)
	if ok {
		t.Fatal("wake phrase must start the transcript")
	}
}

func TestExactModeRejectsApproximateWakePhrase(t *testing.T) {
	ok, _, _ := Match("hey voise turn on the light", []string{"hey voice"}, 0)
	if ok {
		t.Fatal("exact mode must reject approximate wake phrases")
	}
}
