package session

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/config"
)

func TestBuildAudioHelper(t *testing.T) {
	marker := slices.Index(os.Args, "--")
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}
	switch os.Args[marker+1] {
	case "capture":
		frame := make([]byte, 640)
		for _, value := range []uint16{1, 1000, 1000, 1000, 1, 1, 2000, 2000, 2000, 1, 1, 1} {
			for i := 0; i < len(frame); i += 2 {
				binary.LittleEndian.PutUint16(frame[i:], value)
			}
			if _, err := os.Stdout.Write(frame); err != nil {
				os.Exit(3)
			}
		}
		time.Sleep(time.Hour)
	case "play":
		file, err := os.Create(os.Args[marker+2])
		if err != nil {
			os.Exit(4)
		}
		if _, err = io.Copy(file, os.Stdin); err != nil {
			os.Exit(5)
		}
		if err = file.Close(); err != nil {
			os.Exit(6)
		}
	}
	os.Exit(0)
}

func TestBuildAudioLegacyAndRecoveryWiring(t *testing.T) {
	c := config.Default()
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.Detector = "stt"
	if _, ok := Build(c).Recorder.(*audio.CommandRecorder); !ok {
		t.Fatal("default build changed recorder behavior")
	}
	c.Input.Recovery = &config.InputRecovery{}
	if _, ok := Build(c).Recorder.(*audio.RecoveringRecorder); !ok {
		t.Fatal("recovery config not wired")
	}
}

func TestBuildRecoveryPreservesSentenceAndRoutesPairedSpeaker(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir() + "/played.wav"
	c := config.Default()
	c.Timers.Enabled = false
	c.Proposals.Enabled = false
	c.Wake.Detector = "stt"
	c.Input.Command = config.Command{"/nonexistent-voice-capture-command"}
	c.Input.TelephonyHID = ""
	c.Input.Recovery = &config.InputRecovery{RetryInterval: config.Duration(100 * time.Millisecond)}
	c.Input.Fallbacks = []config.InputFallback{{Device: "backup", Command: config.Command{executable, "-test.run=^TestBuildAudioHelper$", "--", "capture"}, Output: &config.Output{Device: "backup-speaker", Command: config.Command{executable, "-test.run=^TestBuildAudioHelper$", "--", "play", output}}}}
	c.Output.Command = config.Command{"/nonexistent-primary-speaker"}
	c.Wake.VAD.Threshold = .01
	c.Wake.VAD.MinSpeech = config.Duration(40 * time.Millisecond)
	c.Wake.VAD.Silence = config.Duration(60 * time.Millisecond)
	c.Wake.VAD.PreRoll = config.Duration(20 * time.Millisecond)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	a := Build(c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pcm, errs := a.Recorder.Stream(ctx)
	utterances := a.VAD.Run(ctx, pcm, a.Recorder.Format())
	select {
	case utterance, ok := <-utterances:
		if !ok {
			t.Fatal("segmenter stopped")
		}
		want := []int16{1, 1000, 1000, 1000, 1, 1, 2000, 2000, 2000, 1, 1, 1}
		if len(utterance.PCM) != len(want)*320 {
			t.Fatalf("sentence has %d samples", len(utterance.PCM))
		}
		for n, value := range want {
			for _, sample := range utterance.PCM[n*320 : (n+1)*320] {
				if sample != value {
					t.Fatalf("frame %d sample=%d want=%d", n, sample, value)
				}
			}
		}
	case <-ctx.Done():
		t.Fatal("no recovery utterance")
	}
	wav := audio.EncodeWAV([]int16{123, -456}, audio.Default())
	if err := a.Player.PlayWAV(ctx, wav); err != nil {
		t.Fatal(err)
	}
	played, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(played, wav) {
		t.Fatal("paired speaker did not receive complete WAV")
	}
	cancel()
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
