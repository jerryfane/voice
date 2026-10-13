package speech

import (
	"strings"
	"testing"
)

func testVocabulary(t *testing.T, c *VocabularyConfig) *Vocabulary {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewVocabulary(c)
}

func TestVocabularyLiteralWholeTerms(t *testing.T) {
	v := testVocabulary(t, &VocabularyConfig{VocabularyScope: VocabularyScope{
		Terms: []string{"Anne", "Sunroom", "Björk", "Kellan", "Café", "東京", "Gamma", "Beta"},
		Replacements: []Replacement{
			{From: "ann", To: "Anne"}, {From: "sun room", To: "Sunroom"},
			{From: "byork", To: "Björk"}, {From: "kelan", To: "Kellan"},
			{From: "cafe\u0301", To: "Café"}, {From: "とうきょう", To: "東京"},
			{From: "alpha", To: "Beta"}, {From: "beta", To: "Gamma"},
		},
	}})
	for _, tc := range []struct{ raw, want string }{
		{"ANN, ann! Ann.", "Anne, Anne! Anne."},
		{"annex Joanne Ann_2 Ann-Marie Ann's Ann’s", "annex Joanne Ann_2 Ann-Marie Ann's Ann’s"},
		{"ann\u0301 anné ann2 2ann", "ann\u0301 anné ann2 2ann"},
		{"do not turn on 2 sun room lights; play BYORK", "do not turn on 2 Sunroom lights; play Björk"},
		{"don't set twenty three timers, no music", "don't set twenty three timers, no music"},
		{"KELAN and KELAN", "Kellan and Kellan"},
		{"CAFE\u0301, とうきょう!", "Café, 東京!"},
		{"café and cafe", "café and cafe"},
		{"sun  room SUN\tROOM sunny room", "sun  room SUN\tROOM sunny room"},
		{"alpha beta alpha", "Beta Gamma Beta"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := v.Correct(tc.raw); got != tc.want {
				t.Fatalf("Correct(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestVocabularyLongestMatchAndExplicitScope(t *testing.T) {
	c := &VocabularyConfig{
		VocabularyScope: VocabularyScope{Terms: []string{"Newark", "New York", "Yorkshire"}, Replacements: []Replacement{
			{From: "new", To: "Newark"}, {From: "new york", To: "New York"}, {From: "york city", To: "Yorkshire"},
		}},
		Contexts: map[string]VocabularyScope{
			"music": {Terms: []string{"Björk"}, Replacements: []Replacement{{From: "byork", To: "Björk"}}},
			"rooms": {Terms: []string{"Sunroom"}, Replacements: []Replacement{{From: "sun room", To: "Sunroom"}}},
		},
	}
	v := testVocabulary(t, c)
	if got := v.Correct("new york city; byork in sun room"); got != "New York city; byork in sun room" {
		t.Fatal(got)
	}
	if got := v.Prompt(); got != "Newark, New York, Yorkshire" {
		t.Fatal(got)
	}
	c.Context = "music"
	v = testVocabulary(t, c)
	if got := v.Correct("byork in sun room"); got != "Björk in sun room" {
		t.Fatal(got)
	}
	if got := v.Prompt(); got != "Newark, New York, Yorkshire, Björk" || v.Context() != "music" {
		t.Fatalf("prompt=%q context=%q", got, v.Context())
	}
	c.Context = "rooms"
	v = testVocabulary(t, c)
	if got := v.Correct("byork in sun room"); got != "byork in Sunroom" {
		t.Fatal(got)
	}
}

func TestVocabularyEmptyAndHintsOnlyNeverRewrite(t *testing.T) {
	raw := "  Don't change 12.5 or TWENTY; Bjork!\n"
	for _, c := range []*VocabularyConfig{nil, {}, {VocabularyScope: VocabularyScope{Terms: []string{"Björk"}}}} {
		v := testVocabulary(t, c)
		if got := v.Correct(raw); got != raw {
			t.Fatalf("raw text changed: %q", got)
		}
	}
}

func TestVocabularyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    VocabularyConfig
	}{
		{"empty term", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{""}}}},
		{"trimmed", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{" Ann"}}}},
		{"control", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Ann\nMarie"}}}},
		{"format", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Ann\u202eMarie"}}}},
		{"invalid UTF8", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"A\xffn"}}}},
		{"too long", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{strings.Repeat("a", 129)}}}},
		{"too many terms", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: make([]string, 257)}}},
		{"undeclared target", VocabularyConfig{VocabularyScope: VocabularyScope{Replacements: []Replacement{{From: "ann", To: "Anne"}}}}},
		{"empty source", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Anne"}, Replacements: []Replacement{{From: "", To: "Anne"}}}}},
		{"duplicate folded source", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Kellan"}, Replacements: []Replacement{{From: "kelan", To: "Kellan"}, {From: "KELAN", To: "Kellan"}}}}},
		{"no-op", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Anne"}, Replacements: []Replacement{{From: "Anne", To: "Anne"}}}}},
		{"unknown context", VocabularyConfig{Context: "music"}},
		{"inactive invalid context", VocabularyConfig{Contexts: map[string]VocabularyScope{"music": {Terms: []string{""}}}}},
		{"global context conflict", VocabularyConfig{VocabularyScope: VocabularyScope{Terms: []string{"Anne"}, Replacements: []Replacement{{From: "ann", To: "Anne"}}}, Contexts: map[string]VocabularyScope{"music": {Replacements: []Replacement{{From: "ANN", To: "Anne"}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); err == nil {
				t.Fatal("invalid vocabulary accepted")
			}
		})
	}
}

func TestVocabularyExplicitOwnerRulesNotLanguageBlacklist(t *testing.T) {
	v := testVocabulary(t, &VocabularyConfig{VocabularyScope: VocabularyScope{
		Terms:        []string{"No Doubt", "Room 2", "Twenty One Pilots"},
		Replacements: []Replacement{{From: "know doubt", To: "No Doubt"}, {From: "room two", To: "Room 2"}, {From: "twenty won pilots", To: "Twenty One Pilots"}},
	}})
	if got := v.Correct("do not play know doubt in room two; 2 twenty won pilots songs"); got != "do not play No Doubt in Room 2; 2 Twenty One Pilots songs" {
		t.Fatal(got)
	}
}
