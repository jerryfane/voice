package wake

import (
	"fmt"
	"os"
	"strings"
)

// SherpaConfig identifies one resident Sherpa-ONNX keyword model.
type SherpaConfig struct {
	Phrases           []string
	Encoder           string
	Decoder           string
	Joiner            string
	Tokens            string
	Keywords          string
	NumThreads        int
	MaxActivePaths    int
	KeywordsScore     float32
	KeywordsThreshold float32
}

// Detector consumes streaming mono PCM and reports only configured keywords.
// Implementations must reset their decoder immediately after a match.
type Detector interface {
	Name() string
	Available() (bool, string)
	Start() error
	Accept(sampleRate int, pcm []int16) (string, error)
	Close()
}

// ResettableDetector discards all streaming state at a capture discontinuity,
// including buffered acoustic features, without unloading the resident model.
// Detectors without this optional interface are closed and started again.
type ResettableDetector interface {
	Reset() error
}

// NewSherpa returns the native implementation for CGO builds and a fail-closed
// unavailable implementation for builds that cannot link Sherpa-ONNX.
func NewSherpa(c SherpaConfig) Detector { return newSherpa(c) }

// Model-file aliases are not authorization: only the owner's configured full
// phrases may open capture. Sherpa aliases conventionally use underscores.
func keywordAliases(phrases []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(phrases))
	for _, phrase := range phrases {
		if name := canonicalKeyword(phrase); name != "" {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}

func canonicalKeyword(name string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(name), "_", " ")), " ")
}

func keywordAllowed(allowed map[string]struct{}, keyword string) bool {
	_, ok := allowed[canonicalKeyword(keyword)]
	return ok
}

func validateSherpaFiles(c SherpaConfig) (bool, string) {
	for _, required := range []struct {
		label string
		path  string
	}{
		{"encoder", c.Encoder},
		{"decoder", c.Decoder},
		{"joiner", c.Joiner},
		{"tokens", c.Tokens},
		{"keywords", c.Keywords},
	} {
		if required.path == "" {
			return false, required.label + " path is empty"
		}
		info, err := os.Stat(required.path)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", required.label, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return false, fmt.Sprintf("%s is not a non-empty regular file", required.label)
		}
	}
	return true, "model files are present"
}
