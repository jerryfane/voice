package audio

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This test binary is also a real capture process, requiring neither arecord
// nor hardware. The -- marker prevents normal test execution entering helpers.
func TestCaptureProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "exit":
		os.Exit(7)
	case "stall":
		time.Sleep(time.Hour)
	case "zero":
		frame := make([]byte, 640)
		for {
			if _, err := os.Stdout.Write(frame); err != nil {
				os.Exit(0)
			}
		}
	case "child":
		child := exec.Command(os.Args[0], "-test.run=^TestCaptureProcessHelper$", "--", "stall")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		frame := make([]byte, 640)
		binary.LittleEndian.PutUint16(frame, 1)
		if _, err := os.Stdout.Write(frame); err != nil {
			os.Exit(9)
		}
		if err := child.Wait(); err != nil {
			os.Exit(10)
		}
	}
	os.Exit(0)
}

func processRecorder(t *testing.T, mode string) *CommandRecorder {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return NewCommandRecorder([]string{executable, "-test.run=^TestCaptureProcessHelper$", "--", mode}, "test", Default(), 320, nil, nil)
}

func TestRecoveryWithActualCommandExitStallAndZero(t *testing.T) {
	for _, mode := range []string{"exit", "stall", "zero"} {
		t.Run(mode, func(t *testing.T) {
			fallback := newRecoveryRecorder("fallback")
			o := recoveryOptions()
			o.StallTimeout = 100 * time.Millisecond
			r, err := NewRecoveringRecorder([]CaptureChoice{{Recorder: processRecorder(t, mode)}, {Recorder: fallback}}, o, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pcm, errs := r.Stream(ctx)
			capture := waitCapture(t, fallback)
			sendCapture(t, capture, pcmFrame(17))
			if got := nextPCM(t, pcm)[0]; got != 17 {
				t.Fatalf("fallback PCM = %d", got)
			}
			endRecovery(t, cancel, pcm, errs, fallback)
		})
	}
}

func TestCommandRecorderCancellationClosesInheritedPipes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pcm, errs := processRecorder(t, "child").Stream(ctx)
	if got := nextPCM(t, pcm)[0]; got != 1 {
		t.Fatalf("helper PCM = %d", got)
	}
	endRecovery(t, cancel, pcm, errs)
}
