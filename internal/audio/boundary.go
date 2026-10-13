package audio

import "context"

// CaptureBoundary synchronizes capture selection with the segmenter. Each frame
// is acknowledged only after segmentation, so a stale idle observation can never
// switch microphones in the middle of a later utterance. A nil PCM frame marks a
// discontinuity and must discard partial utterances before being acknowledged.
// One recorder stream and one segmenter share a boundary for their lifetime.
type CaptureBoundary struct {
	acks chan bool
}

func NewCaptureBoundary() *CaptureBoundary {
	return &CaptureBoundary{acks: make(chan bool)}
}

// Acknowledge reports whether the just-consumed frame ended at a safe, quiet
// boundary. False means cancellation; callers must then stop consuming frames.
func (b *CaptureBoundary) Acknowledge(ctx context.Context, idle bool) bool {
	select {
	case b.acks <- idle:
		return true
	case <-ctx.Done():
		return false
	}
}

// Wait receives the acknowledgement for the last delivered frame.
func (b *CaptureBoundary) Wait(ctx context.Context) (idle bool, ok bool) {
	select {
	case idle = <-b.acks:
		return idle, true
	case <-ctx.Done():
		return false, false
	}
}
