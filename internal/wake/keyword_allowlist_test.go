package wake

import "testing"

func TestKeywordAuthorizationRequiresConfiguredFullPhrase(t *testing.T) {
	allowed := keywordAliases([]string{"Hey Voice", "hey boys", "hello computer"})
	for _, tc := range []struct {
		keyword string
		want    bool
	}{
		{"HEY_VOICE", true}, {"hey voice", true}, {"HEY_BOYS", true}, {"HELLO_COMPUTER", true},
		{"VOICE", false}, {"BOYS", false}, {"COMPUTER", false}, {"HEY", false}, {"HEY_VOICES", false}, {"SOME_HEY_VOICE_WORDS", false}, {"", false},
	} {
		if got := keywordAllowed(allowed, tc.keyword); got != tc.want {
			t.Errorf("authorization for %q=%v, want %v", tc.keyword, got, tc.want)
		}
	}
	if keywordAllowed(keywordAliases(nil), "HEY_VOICE") {
		t.Fatal("empty allowlist authorized a wake")
	}
	// A single word is permitted only if the owner actually configured it.
	if !keywordAllowed(keywordAliases([]string{"voice"}), "VOICE") {
		t.Fatal("explicit owner phrase was rejected")
	}
}
