package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/jerryfane/voice/internal/faults"
	"github.com/jerryfane/voice/internal/hid"
	"github.com/jerryfane/voice/internal/proc"
)

// CommandRecorder captures raw signed 16-bit little-endian PCM from an
// external command. The command writes PCM to stdout.
type CommandRecorder struct {
	argv         []string
	device       string
	format       Format
	frameSamples int
	telephony    *hid.Telephony
	faults       faults.Reporter
}

// NewCommandRecorder builds a recorder. frameSamples controls the size of each
// emitted buffer; 20 ms frames are a good VAD default. tel may be nil when the
// input needs no telephony handshake. report receives failures that happen
// after Stream has returned - notably clearing the off-hook report when
// capture ends, which has no caller left to tell.
func NewCommandRecorder(argv []string, device string, f Format, frameSamples int, tel *hid.Telephony, report faults.Reporter) *CommandRecorder {
	if frameSamples <= 0 {
		frameSamples = f.SampleRate / 50
	}
	return &CommandRecorder{argv: argv, device: device, format: f, frameSamples: frameSamples, telephony: tel, faults: report}
}

func (r *CommandRecorder) Format() Format { return r.format }
func (r *CommandRecorder) Describe() string {
	return fmt.Sprintf("command input %q on %s (%d Hz mono)", r.argv[0], r.device, r.format.SampleRate)
}

// setOffHook sends the standard USB HID telephony output report used by
// speakerphones such as the Anker PowerConf: without it they stream digital
// silence. The report is cleared when capture ends. A udev rule should grant
// the Voice service access to the hidraw node.
//
// The write goes through hid.Telephony, which retains the whole report, so the
// listening indicator can share report 2 without clearing the off-hook bit.
func (r *CommandRecorder) setOffHook(on bool) error {
	if r.telephony == nil {
		return nil
	}
	done, err := r.telephony.Set(hid.PageLED, hid.LEDOffHook, on)
	if err != nil {
		return fmt.Errorf("set telephony off-hook=%v: %w", on, err)
	}
	if done {
		return nil
	}
	// The descriptor did not advertise the LED, which happens when sysfs is
	// unreadable. Fall back to the standard headset report, which states its
	// own length so the write is not short.
	if err := r.telephony.SetReportBit(hid.StandardOffHook, on); err != nil {
		return fmt.Errorf("set telephony off-hook=%v: %w", on, err)
	}
	return nil
}

func (r *CommandRecorder) Stream(ctx context.Context) (<-chan []int16, <-chan error) {
	out := make(chan []int16, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		if len(r.argv) == 0 {
			errCh <- errors.New("capture command is empty")
			return
		}
		if err := r.setOffHook(true); err != nil {
			errCh <- err
			return
		}
		// Clearing off-hook happens as capture unwinds, when nothing is left
		// to return an error to: a silent failure here leaves the
		// speakerphone off hook with its light on.
		defer func() {
			if err := r.setOffHook(false); err != nil && r.faults != nil {
				r.faults.Report(err)
			}
		}()

		vars := map[string]string{
			"device": r.device, "rate": strconv.Itoa(r.format.SampleRate),
			"channels": strconv.Itoa(r.format.Channels),
		}
		argv := proc.Expand(r.argv, vars)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errCh <- err
			return
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			errCh <- fmt.Errorf("start capture: %w", err)
			return
		}

		frame := make([]byte, r.frameSamples*2)
		for {
			_, err := io.ReadFull(stdout, frame)
			if err != nil {
				if err != io.EOF && err != io.ErrUnexpectedEOF && ctx.Err() == nil {
					errCh <- fmt.Errorf("capture read: %w", err)
				}
				break
			}
			samples := make([]int16, r.frameSamples)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(frame[i*2:]))
			}
			select {
			case out <- samples:
			case <-ctx.Done():
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
		err = cmd.Wait()
		if err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("capture command: %w: %s", err, stderr.String())
		}
	}()
	return out, errCh
}

// CommandPlayer renders WAV data by writing it to an external command's stdin.
type CommandPlayer struct {
	argv   []string
	device string
	mu     sync.Mutex
	cmd    *exec.Cmd
}

func NewCommandPlayer(argv []string, device string) *CommandPlayer {
	return &CommandPlayer{argv: argv, device: device}
}

func (p *CommandPlayer) Describe() string {
	if len(p.argv) == 0 {
		return "empty playback command"
	}
	return fmt.Sprintf("command output %q on %s", p.argv[0], p.device)
}

func (p *CommandPlayer) PlayWAV(ctx context.Context, wav []byte) error {
	if len(p.argv) == 0 {
		return errors.New("playback command is empty")
	}
	argv := proc.Expand(p.argv, map[string]string{"device": p.device})
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(wav)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()
	err := cmd.Run()
	p.mu.Lock()
	p.cmd = nil
	p.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("playback: %w: %s", err, stderr.String())
	}
	return ctx.Err()
}

func (p *CommandPlayer) PlayPCM(ctx context.Context, pcm []int16, f Format) error {
	return p.PlayWAV(ctx, EncodeWAV(pcm, f))
}

func (p *CommandPlayer) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// EncodeWAV wraps signed 16-bit PCM in a minimal RIFF/WAVE container.
func EncodeWAV(pcm []int16, f Format) []byte {
	wav := wavBuffer(len(pcm)*2, f)
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(wav[44+i*2:], uint16(sample))
	}
	return wav
}

// EncodePCM16WAV wraps little-endian signed 16-bit PCM bytes without first
// expanding them into a second sample slice.
func EncodePCM16WAV(pcm []byte, f Format) []byte {
	wav := wavBuffer(len(pcm), f)
	copy(wav[44:], pcm)
	return wav
}

func wavBuffer(dataLen int, f Format) []byte {
	wav := make([]byte, 44+dataLen)
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+dataLen))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], uint16(f.Channels))
	binary.LittleEndian.PutUint32(wav[24:28], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(f.SampleRate*f.Channels*2))
	binary.LittleEndian.PutUint16(wav[32:34], uint16(f.Channels*2))
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(dataLen))
	return wav
}

// PrependWAVSilence keeps a playback device open before speech begins. USB
// speakerphones can discard their first audio frames while waking, which
// otherwise clips the first phoneme even though playback exits successfully.
func PrependWAVSilence(wav []byte, lead time.Duration) ([]byte, error) {
	if lead <= 0 {
		return wav, nil
	}
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("audio is not RIFF/WAVE")
	}

	var byteRate uint32
	var blockAlign uint16
	for offset := 12; offset+8 <= len(wav); {
		size := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		data := offset + 8
		end := data + size
		if size < 0 || end < data || end > len(wav) {
			return nil, errors.New("WAVE chunk exceeds payload")
		}
		switch string(wav[offset : offset+4]) {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("WAVE fmt chunk is too short")
			}
			if binary.LittleEndian.Uint16(wav[data:data+2]) != 1 {
				return nil, errors.New("WAVE audio is not PCM")
			}
			byteRate = binary.LittleEndian.Uint32(wav[data+8 : data+12])
			blockAlign = binary.LittleEndian.Uint16(wav[data+12 : data+14])
			if byteRate == 0 || blockAlign == 0 {
				return nil, errors.New("WAVE fmt chunk has an invalid rate")
			}
		case "data":
			if byteRate == 0 || blockAlign == 0 {
				return nil, errors.New("WAVE data precedes its fmt chunk")
			}
			silence := uint64(byteRate) * uint64(lead) / uint64(time.Second)
			alignment := uint64(blockAlign)
			if alignment%2 != 0 {
				alignment *= 2
			}
			silence -= silence % alignment
			if silence == 0 {
				return wav, nil
			}
			maxUint32 := uint64(^uint32(0))
			riffSize := uint64(binary.LittleEndian.Uint32(wav[4:8]))
			if uint64(size)+silence > maxUint32 || riffSize+silence > maxUint32 {
				return nil, errors.New("padded WAVE exceeds RIFF size limits")
			}

			n := int(silence)
			out := make([]byte, len(wav)+n)
			copy(out, wav[:data])
			copy(out[data+n:], wav[data:])
			binary.LittleEndian.PutUint32(out[offset+4:offset+8], uint32(uint64(size)+silence))
			binary.LittleEndian.PutUint32(out[4:8], uint32(riffSize+silence))
			return out, nil
		}
		offset = end + size%2
	}
	return nil, errors.New("WAVE has no data chunk")
}
