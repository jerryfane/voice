// Package faults carries failures that have nowhere to be returned.
//
// Background goroutines, deferred cleanup and fire-and-forget device writes all
// produce errors after their caller has moved on. Left alone these become
// `_ = doThing()`, and the failure is gone: a timer that came back after a
// restart, a speakerphone left off hook. Go cannot forbid discarding an error,
// so the enforcement here is structural in a different way - every type that
// owns such a path takes a Reporter in its constructor, so a new emit path
// inside it already has somewhere to send failures and needs no new wiring.
package faults

import "log"

// Reporter receives a failure that cannot be returned.
type Reporter interface {
	Report(error)
}

// Func adapts a function to a Reporter. A nil Func reports nothing, which
// keeps zero values usable in tests.
type Func func(error)

func (f Func) Report(err error) {
	if f == nil || err == nil {
		return
	}
	f(err)
}

// Log reports failures to a logger, prefixed with the subsystem that produced
// them.
func Log(l *log.Logger, subsystem string) Reporter {
	return Func(func(err error) {
		if l == nil || err == nil {
			return
		}
		l.Printf("%s: %v", subsystem, err)
	})
}

// Discard drops failures. It exists so that ignoring them is a visible choice
// at the call site rather than an underscore.
var Discard Reporter = Func(nil)
