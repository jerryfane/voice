package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/speech"
)

func TestVocabularyDefaultsRemainOptIn(t *testing.T) {
	c := Default()
	if c.STT.Vocabulary != nil {
		t.Fatal("default vocabulary must not invent personal terms")
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"vocabulary"`) {
		t.Fatal("absent vocabulary changed default serialized config")
	}
	c.STT.Vocabulary = &speech.VocabularyConfig{}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadScopedVocabulary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"stt":{"vocabulary":{"terms":["Sunroom"],"replacements":[{"from":"sun room","to":"Sunroom"}],"context":"music","contexts":{"music":{"terms":["Björk"],"replacements":[{"from":"byork","to":"Björk"}]}}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	v := speech.NewVocabulary(c.STT.Vocabulary)
	if v.Prompt() != "Sunroom, Björk" || v.Correct("byork in sun room") != "Björk in Sunroom" {
		t.Fatalf("decoded vocabulary=%+v", c.STT.Vocabulary)
	}
	c.STT.Vocabulary.Context = "unconfigured"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "stt.vocabulary") {
		t.Fatalf("invalid vocabulary error=%v", err)
	}
}

func TestLoadRejectsVocabularyTypos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"stt":{"vocabulary":{"replacement":[{"from":"sun room","to":"Sunroom"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("misspelled vocabulary rule silently ignored")
	}
}
