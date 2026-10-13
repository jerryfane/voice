package speech

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// VocabularyConfig is owner-supplied terminology, not an instruction to a model.
// Context is selected explicitly in config; speech never guesses or changes it.
// Empty configuration leaves transcripts and engine requests unchanged.
type VocabularyConfig struct {
	VocabularyScope
	Context  string                     `json:"context,omitempty"`
	Contexts map[string]VocabularyScope `json:"contexts,omitempty"`
}

type VocabularyScope struct {
	Terms        []string      `json:"terms,omitempty"`
	Replacements []Replacement `json:"replacements,omitempty"`
}

// Replacement changes only a literal whole term to a declared canonical term.
// Owners are responsible for the meaning of explicitly configured rules,
// including any deliberate changes to commands, numbers or negation. No such
// changes are inferred, and terms alone never create replacement rules.
type Replacement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Validate rejects ambiguous rules even in inactive contexts. A context may
// add rules, but cannot override a global source. Targets must be declared in
// that scope or globally, so arbitrary sentence rewrites are not implicit.
func (c *VocabularyConfig) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Contexts) > 32 {
		return fmt.Errorf("at most 32 contexts are allowed")
	}
	if c.Context != "" {
		if _, ok := c.Contexts[c.Context]; !ok {
			return fmt.Errorf("unknown context %q", c.Context)
		}
	}
	if err := validateVocabularyScope(c.VocabularyScope, VocabularyScope{}); err != nil {
		return fmt.Errorf("global: %w", err)
	}
	names := make([]string, 0, len(c.Contexts))
	for name := range c.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateVocabularyTerm(name); err != nil {
			return fmt.Errorf("context %q: %w", name, err)
		}
		if err := validateVocabularyScope(c.Contexts[name], c.VocabularyScope); err != nil {
			return fmt.Errorf("context %q: %w", name, err)
		}
	}
	return nil
}

func validateVocabularyScope(scope, global VocabularyScope) error {
	if len(scope.Terms) > 256 || len(scope.Replacements) > 256 {
		return fmt.Errorf("at most 256 terms and 256 replacements per scope are allowed")
	}
	terms := make(map[string]bool, len(global.Terms)+len(scope.Terms))
	for _, term := range global.Terms {
		terms[term] = true
	}
	for _, term := range scope.Terms {
		if err := validateVocabularyTerm(term); err != nil {
			return fmt.Errorf("term %q: %w", term, err)
		}
		terms[term] = true
	}
	sources := make(map[string]bool, len(global.Replacements)+len(scope.Replacements))
	for _, rule := range global.Replacements {
		sources[foldTerm(rule.From)] = true
	}
	for _, rule := range scope.Replacements {
		if err := validateVocabularyTerm(rule.From); err != nil {
			return fmt.Errorf("replacement source %q: %w", rule.From, err)
		}
		if !terms[rule.To] {
			return fmt.Errorf("replacement target %q must exactly match a declared term", rule.To)
		}
		key := foldTerm(rule.From)
		if sources[key] {
			return fmt.Errorf("duplicate replacement source %q", rule.From)
		}
		sources[key] = true
		if rule.From == rule.To {
			return fmt.Errorf("replacement %q does not change anything", rule.From)
		}
	}
	return nil
}

func validateVocabularyTerm(term string) error {
	if !utf8.ValidString(term) || term == "" || strings.TrimSpace(term) != term || utf8.RuneCountInString(term) > 128 || len(strings.Fields(term)) > 8 {
		return fmt.Errorf("must be valid UTF-8, nonempty, trimmed, at most 128 characters and 8 words")
	}
	first, _ := utf8.DecodeRuneInString(term)
	last, _ := utf8.DecodeLastRuneInString(term)
	if !wordRune(first) || !wordRune(last) {
		return fmt.Errorf("must start and end with a letter, mark or number")
	}
	for _, r := range term {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
			return fmt.Errorf("control, format and non-space whitespace characters are not allowed")
		}
	}
	return nil
}

func foldRune(r rune) rune {
	min := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		if next < min {
			min = next
		}
	}
	return min
}

func foldTerm(term string) string {
	return strings.Map(foldRune, term)
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r)
}

// Apostrophes, hyphens and connector punctuation belong to terms: Ann must
// not rewrite Ann's, Ann-Marie or Ann_2. Matching does not normalize accents,
// punctuation or whitespace, and never uses fuzzy or substring replacement.
func termRune(r rune) bool {
	return wordRune(r) || unicode.Is(unicode.Pc, r) || unicode.Is(unicode.Pd, r) || r == '\'' || r == '’'
}

type vocabularyRule struct {
	from []rune
	to   string
}

// Vocabulary is immutable after construction and safe for concurrent use.
type Vocabulary struct {
	context string
	prompt  string
	rules   map[rune][]vocabularyRule
}

// NewVocabulary compiles a config already accepted by Validate (as part of
// config loading). Nil and empty configurations produce a no-op vocabulary.
func NewVocabulary(c *VocabularyConfig) *Vocabulary {
	if c == nil {
		return nil
	}
	v := &Vocabulary{context: c.Context, rules: make(map[rune][]vocabularyRule)}
	scopes := []VocabularyScope{c.VocabularyScope}
	if c.Context != "" {
		scopes = append(scopes, c.Contexts[c.Context])
	}
	var terms []string
	seen := make(map[string]bool)
	for _, scope := range scopes {
		for _, term := range scope.Terms {
			key := foldTerm(term)
			if !seen[key] {
				terms = append(terms, term)
				seen[key] = true
			}
		}
		for _, rule := range scope.Replacements {
			from := []rune(foldTerm(rule.From))
			v.rules[from[0]] = append(v.rules[from[0]], vocabularyRule{from: from, to: rule.To})
		}
	}
	for first, rules := range v.rules {
		sort.SliceStable(rules, func(i, j int) bool { return len(rules[i].from) > len(rules[j].from) })
		v.rules[first] = rules
	}
	v.prompt = strings.Join(terms, ", ")
	return v
}

// Prompt is terminology only; engines without verified hint support ignore it.
func (v *Vocabulary) Prompt() string {
	if v == nil {
		return ""
	}
	return v.prompt
}

func (v *Vocabulary) Context() string {
	if v == nil {
		return ""
	}
	return v.context
}

// Correct scans the ORIGINAL text left-to-right, taking the longest matching
// term at each boundary. Replacement output is never rescanned. Outside each
// matched span every byte (case, punctuation, quantities, negation) is retained.
// The unchanged path returns the original string without allocating.
func (v *Vocabulary) Correct(raw string) string {
	if v == nil || len(v.rules) == 0 {
		return raw
	}
	var out strings.Builder
	copied := 0
	var previous rune
	for pos := 0; pos < len(raw); {
		r, size := utf8.DecodeRuneInString(raw[pos:])
		end := pos
		if !termRune(previous) {
			for _, rule := range v.rules[foldRune(r)] {
				cursor := pos
				matched := 0
				for _, want := range rule.from {
					if cursor == len(raw) {
						break
					}
					got, n := utf8.DecodeRuneInString(raw[cursor:])
					if foldRune(got) != want {
						break
					}
					cursor += n
					matched++
				}
				// Count matched runes, not bytes: Unicode folding may change
				// UTF-8 width (for example Kelvin sign and Latin K).
				if matched != len(rule.from) {
					continue
				}
				end = cursor
				next, _ := utf8.DecodeRuneInString(raw[end:])
				if end < len(raw) && termRune(next) {
					end = pos
					continue
				}
				if _, err := out.WriteString(raw[copied:pos]); err != nil {
					panic(err) // strings.Builder writes cannot fail.
				}
				if _, err := out.WriteString(rule.to); err != nil {
					panic(err) // strings.Builder writes cannot fail.
				}
				copied = end
				break
			}
		}
		if end > pos {
			previous, _ = utf8.DecodeLastRuneInString(raw[pos:end])
			pos = end
		} else {
			previous = r
			pos += size
		}
	}
	if copied == 0 {
		return raw
	}
	if _, err := out.WriteString(raw[copied:]); err != nil {
		panic(err) // strings.Builder writes cannot fail.
	}
	return out.String()
}
