//go:build cgo

package wake

import (
	"errors"
	"fmt"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

type sherpaDetector struct {
	config  SherpaConfig
	spotter *sherpa.KeywordSpotter
	stream  *sherpa.OnlineStream
	floats  []float32
}

func newSherpa(c SherpaConfig) Detector { return &sherpaDetector{config: c} }

func (*sherpaDetector) Name() string { return "sherpa-onnx keyword spotter" }

func (d *sherpaDetector) Available() (bool, string) {
	return validateSherpaFiles(d.config)
}

func (d *sherpaDetector) Start() error {
	if d.spotter != nil {
		return nil
	}
	if ok, why := d.Available(); !ok {
		return errors.New(why)
	}
	c := sherpa.KeywordSpotterConfig{}
	c.FeatConfig.SampleRate = 16000
	c.FeatConfig.FeatureDim = 80
	c.ModelConfig.Transducer.Encoder = d.config.Encoder
	c.ModelConfig.Transducer.Decoder = d.config.Decoder
	c.ModelConfig.Transducer.Joiner = d.config.Joiner
	c.ModelConfig.Tokens = d.config.Tokens
	c.ModelConfig.NumThreads = d.config.NumThreads
	c.ModelConfig.Provider = "cpu"
	c.MaxActivePaths = d.config.MaxActivePaths
	c.KeywordsFile = d.config.Keywords
	c.KeywordsScore = d.config.KeywordsScore
	c.KeywordsThreshold = d.config.KeywordsThreshold

	d.spotter = sherpa.NewKeywordSpotter(&c)
	if d.spotter == nil {
		return errors.New("Sherpa-ONNX could not create the keyword spotter")
	}
	d.stream = sherpa.NewKeywordStream(d.spotter)
	if d.stream == nil {
		sherpa.DeleteKeywordSpotter(d.spotter)
		d.spotter = nil
		return errors.New("Sherpa-ONNX could not create the keyword stream")
	}
	return nil
}

func (d *sherpaDetector) Accept(sampleRate int, pcm []int16) (string, error) {
	if d.stream == nil || d.spotter == nil {
		return "", errors.New("keyword spotter is not started")
	}
	if sampleRate != 16000 {
		return "", fmt.Errorf("keyword spotter requires 16000 Hz audio, got %d", sampleRate)
	}
	if len(pcm) == 0 {
		return "", nil
	}
	if cap(d.floats) < len(pcm) {
		d.floats = make([]float32, len(pcm))
	} else {
		d.floats = d.floats[:len(pcm)]
	}
	for i, sample := range pcm {
		d.floats[i] = float32(sample) / 32768
	}
	d.stream.AcceptWaveform(sampleRate, d.floats)
	for d.spotter.IsReady(d.stream) {
		d.spotter.Decode(d.stream)
		result := d.spotter.GetResult(d.stream)
		if result != nil && result.Keyword != "" {
			d.spotter.Reset(d.stream)
			return result.Keyword, nil
		}
	}
	return "", nil
}

func (d *sherpaDetector) Close() {
	if d.stream != nil {
		sherpa.DeleteOnlineStream(d.stream)
		d.stream = nil
	}
	if d.spotter != nil {
		sherpa.DeleteKeywordSpotter(d.spotter)
		d.spotter = nil
	}
	d.floats = nil
}
