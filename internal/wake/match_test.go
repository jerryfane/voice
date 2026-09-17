package wake

import "testing"

func TestMatchExtractsInlineCommand(t *testing.T) {
	ok, cmd, phrase := Match("Hey, HERDR! turn the bedroom light purple", []string{"hey herdr"}, 0.2)
	if !ok || cmd != "turn the bedroom light purple" || phrase != "hey herdr" {
		t.Fatalf("got ok=%v cmd=%q phrase=%q", ok, cmd, phrase)
	}
}
func TestMatchToleratesSTTSpelling(t *testing.T) {
	ok, _, _ := Match("hey herder what time is it", []string{"hey herdr"}, 0.25)
	if !ok {
		t.Fatal("expected fuzzy match")
	}
}
func TestNoMatchInsideUnrelatedSpeech(t *testing.T) {
	ok, _, _ := Match("the cattle herder went home", []string{"hey herdr"}, 0.25)
	if ok {
		t.Fatal("unexpected wake match")
	}
}
