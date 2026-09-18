//go:build !cgo

package wake

import "errors"

type unavailableSherpa struct{ config SherpaConfig }

func newSherpa(c SherpaConfig) Detector { return &unavailableSherpa{config: c} }

func (*unavailableSherpa) Name() string { return "sherpa-onnx keyword spotter" }
func (*unavailableSherpa) Available() (bool, string) {
	return false, "Voice was built without CGO/Sherpa-ONNX support"
}
func (d *unavailableSherpa) Start() error {
	_, why := d.Available()
	return errors.New(why)
}
func (*unavailableSherpa) Accept(int, []int16) (string, error) {
	return "", errors.New("Sherpa-ONNX keyword spotter is unavailable")
}
func (*unavailableSherpa) Close() {}
