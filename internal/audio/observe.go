package audio

import "context"

// ObserveStream measures the actual logical capture stream immediately before
// segmentation. It preserves frame identity and discontinuities; observation
// callbacks must not block or retain samples. Delivery is tracked separately so
// a stuck consumer cannot masquerade as a stalled capture source.
func ObserveStream(ctx context.Context, in <-chan []int16, received func([]int16), delivered func()) <-chan []int16 {
	out := make(chan []int16)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case pcm, ok := <-in:
				if !ok {
					return
				}
				received(pcm)
				select {
				case out <- pcm:
					if len(pcm) != 0 {
						delivered()
					}
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
