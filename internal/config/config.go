// Package config is Voice's single source of truth for how a deployment is
// wired: which microphone, speaker, speech engines, agent command, and devices
// exist.
//
// The format is JSON, on purpose. Voice has no third-party dependencies, so
// adding YAML or TOML would mean vendoring a parser into a binary whose whole
// selling point is that it is one self-contained file. `voice init` writes a
// fully populated config with every default spelled out, so nobody has to
// author JSON from memory.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/audio"
	"github.com/jerryfane/voice/internal/feedback"
)

// Command is an argv template. Placeholders are expanded by internal/proc:
//
//	{device}  input/output device id        {rate}    sample rate
//	{channels} channel count               {file}    path to a temp wav/raw file
//	{model}   engine model path            {text}    text to synthesize
//	{prompt}  full prompt for the agent    {out}     path the engine should write
type Command []string

// Config is the whole deployment.
type Config struct {
	// Input is the microphone. Any device the host exposes works.
	Input Input `json:"input"`
	// Output is the speaker.
	Output Output `json:"output"`
	// Wake controls when Voice starts listening for a command.
	Wake Wake `json:"wake"`
	// Feedback is the local acknowledgement played and shown when the wake
	// gate accepts a phrase. It never involves the model.
	Feedback Feedback `json:"feedback"`
	// Timers configures local spoken timers, which also never involve it.
	Timers Timers `json:"timers"`
	// STT is speech-to-text.
	STT Engine `json:"stt"`
	// TTS is text-to-speech.
	TTS Engine `json:"tts"`
	// Brain is the reasoning layer.
	Brain Brain `json:"brain"`
	// Lights are Zengge/Magic Home controllers (local protocol, port 5577).
	Lights []Light `json:"lights"`
	// TVs are HDMI-CEC displays.
	TVs []TV `json:"tvs"`
}

// Feedback configures the immediate acknowledgement of an accepted wake
// phrase. It is emitted by the session layer, so it works with no network and
// no model.
type Feedback struct {
	// Light selects which speakerphone LED shows that Voice is listening:
	// "auto" takes the first indicator the configured input.telephony_hid
	// device advertises, "none" disables the light, or name one explicitly
	// ("mic", "ring", "mute", "hold") after looking at which one reads as
	// listening on your hardware. The off-hook LED is not offered: capture
	// holds it for the whole session to keep the microphone un-gated.
	// Unsupported hardware only costs the light, never command handling.
	Light string `json:"light"`
	// Sound is the acknowledgement earcon: "chime", "blip", "two-up", or
	// "none" to stay silent. The waveform is generated, not a bundled asset.
	Sound string `json:"sound"`
	// Volume scales the earcon between 0 (silent) and 1 (full scale).
	Volume float64 `json:"volume"`
}

// Timers configures local spoken timers.
type Timers struct {
	// Enabled answers "set a timer for ten minutes" in the session layer.
	// Disabling it sends the request to the agent instead, which cannot
	// deliver an announcement when the model or network is unavailable.
	Enabled bool `json:"enabled"`
	// File is the SQLite database holding the timer set across restarts.
	// Empty uses $XDG_STATE_HOME/voice/timers.db, or
	// ~/.local/state/voice/timers.db.
	// The directory must be writable by the account Voice runs as.
	File string `json:"file,omitempty"`
}

// Input describes audio capture.
type Input struct {
	// Device is passed to the capture command as {device}. "default" uses the
	// host default; `voice devices audio` lists the alternatives.
	Device string `json:"device"`
	// Command captures signed 16-bit mono PCM to stdout, forever.
	Command Command `json:"command"`
	// SampleRate must match what the STT engine expects (16000 for whisper).
	SampleRate int `json:"sample_rate"`
	// TelephonyHID is an optional hidraw path for USB speakerphones that gate
	// capture until the host signals "off hook" (for example Anker PowerConf).
	// Voice sends the standard report before capture and clears it on stop.
	TelephonyHID string `json:"telephony_hid,omitempty"`
}

// Output describes audio playback.
type Output struct {
	Device string `json:"device"`
	// Command plays a RIFF/WAVE payload supplied on stdin.
	Command Command `json:"command"`
}

// Wake controls wake-phrase detection.
type Wake struct {
	// Phrases are accepted wake phrases, lowercase. Multiple spellings may be
	// listed when speech recognition commonly confuses the chosen phrase.
	Phrases []string `json:"phrases"`
	// Fuzz is the maximum normalised edit distance (0-1) still counted as a
	// match. 0 demands an exact transcript; 0.25 tolerates one wrong character
	// in four. Raise it if the recogniser keeps mangling the name.
	Fuzz float64 `json:"fuzz"`
	// Detector selects the strategy: "stt" transcribes every speech segment
	// and matches the phrase (no extra dependency, works today); "external"
	// runs Command and treats any line on stdout as a detection.
	Detector string  `json:"detector"`
	Command  Command `json:"command,omitempty"`
	// VAD tunes speech segmentation.
	VAD VAD `json:"vad"`
}

// VAD tunes the energy-based speech segmenter.
type VAD struct {
	// Threshold is the RMS level (0-1, of full scale) above which a frame
	// counts as speech. 0 means auto-calibrate from ambient noise.
	Threshold float64 `json:"threshold"`
	// MinSpeech is the shortest run of speech treated as an utterance, which
	// rejects door clicks and keyboard taps.
	MinSpeech Duration `json:"min_speech"`
	// Silence is how much quiet ends an utterance.
	Silence Duration `json:"silence"`
	// MaxUtterance caps a single utterance so a running tap never produces an
	// unbounded buffer.
	MaxUtterance Duration `json:"max_utterance"`
	// PreRoll is how much audio before the trigger is kept, so the first
	// syllable is not clipped.
	PreRoll Duration `json:"pre_roll"`
}

// Engine is an external speech program.
type Engine struct {
	// Name is informational ("whisper.cpp", "piper").
	Name string `json:"name"`
	// Command is the argv template.
	Command Command `json:"command"`
	// Model is substituted as {model}.
	Model string `json:"model,omitempty"`
	// Timeout bounds one invocation.
	Timeout Duration `json:"timeout"`
}

// Brain configures reasoning.
type Brain struct {
	// Mode is "plan" (model returns JSON) or "agent" (a persistent session with
	// its own tools and memory).
	Mode string `json:"mode"`
	// Command runs the model. The prompt is substituted as {prompt}; if no
	// {prompt} placeholder appears, the prompt is written to stdin instead.
	Command Command `json:"command"`
	// Timeout bounds one invocation.
	Timeout Duration `json:"timeout"`
	// Persona is prepended to the agent prompt: how Voice should sound.
	Persona string `json:"persona"`
	// Rules enables the local no-model fast path for trivial queries.
	Rules bool `json:"rules"`
}

// Light is a Zengge/Magic Home controller.
type Light struct {
	// ID is what you say and type: "bedroom".
	ID string `json:"id"`
	// Host is the controller's address. Give it a static DHCP lease.
	Host string `json:"host"`
	// Port defaults to 5577.
	Port int `json:"port,omitempty"`
	// Protocol selects the frame format: "auto" probes the device type byte,
	// "9byte" is the modern LEDENET frame, "8byte" the legacy one.
	Protocol string `json:"protocol,omitempty"`
}

// TV is an HDMI-CEC display.
type TV struct {
	ID string `json:"id"`
	// Adapter is the CEC character device, e.g. /dev/cec0.
	Adapter string `json:"adapter"`
	// LogicalAddress is the CEC target; 0 is always the TV.
	LogicalAddress int `json:"logical_address"`
}

// Duration is a time.Duration that marshals as a human string ("1.5s", "800ms")
// so configs stay readable.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a string ("800ms") or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"800ms\" or a nanosecond count")
	}
	*d = Duration(n)
	return nil
}

// Default returns a config that works on a stock Linux box with ALSA, using
// whisper.cpp for recognition and Piper for speech.
func Default() Config {
	return Config{
		Input: Input{
			Device:     "default",
			SampleRate: 16000,
			Command: Command{
				"arecord", "-D", "{device}", "-q",
				"-f", "S16_LE", "-r", "{rate}", "-c", "{channels}",
				"-t", "raw", "-",
			},
		},
		Output: Output{
			Device:  "default",
			Command: Command{"aplay", "-D", "{device}", "-q", "-"},
		},
		Wake: Wake{
			Phrases:  []string{"hey voice"},
			Fuzz:     0,
			Detector: "stt",
			VAD: VAD{
				Threshold:    0,
				MinSpeech:    Duration(250 * time.Millisecond),
				Silence:      Duration(700 * time.Millisecond),
				MaxUtterance: Duration(12 * time.Second),
				PreRoll:      Duration(300 * time.Millisecond),
			},
		},
		Feedback: Feedback{
			Light:  "auto",
			Sound:  audio.EarconChime,
			Volume: 0.35,
		},
		Timers: Timers{Enabled: true},
		STT: Engine{
			Name:    "whisper.cpp",
			Model:   "models/ggml-base.en.bin",
			Timeout: Duration(30 * time.Second),
			Command: Command{
				"whisper-cli", "-m", "{model}", "-f", "{file}",
				"--no-timestamps", "--no-prints", "-t", "3", "-l", "en",
				"--prompt", "Hey Voice.",
			},
		},
		TTS: Engine{
			Name:    "piper",
			Model:   "models/en_US-amy-medium.onnx",
			Timeout: Duration(30 * time.Second),
			Command: Command{
				"piper", "--model", "{model}", "--output_file", "-",
			},
		},
		Brain: Brain{
			Mode:    "agent",
			Timeout: Duration(10 * time.Minute),
			Command: Command{"voice-agent-run", "{prompt}"},
			Persona: "You are Voice, a concise personal agent. Complete the user's request with your tools, remember useful context across turns, and answer in one or two natural spoken sentences.",
			Rules:   false,
		},
		Lights: []Light{},
		TVs:    []TV{},
	}
}

// Path returns the config file location, honouring VOICE_CONFIG and
// XDG_CONFIG_HOME before falling back to ~/.config/voice/config.json.
func Path() (string, error) {
	if p := os.Getenv("VOICE_CONFIG"); p != "" {
		return p, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "voice", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "voice", "config.json"), nil
}

// StatePath returns where mutable runtime state lives, honouring
// XDG_STATE_HOME before falling back to ~/.local/state/voice. The service unit
// points XDG_STATE_HOME at its systemd StateDirectory.
func StatePath(name string) (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "voice", name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "voice", name), nil
}

// Load reads the config at path, or at the default location when path is empty.
// Unknown fields are rejected so a typo in a key name is reported instead of
// silently ignored.
func Load(path string) (Config, string, error) {
	var err error
	if path == "" {
		if path, err = Path(); err != nil {
			return Config{}, "", err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, path, err
	}
	cfg := Default()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, path, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, path, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, path, nil
}

// Save writes cfg to path, creating parent directories.
func Save(cfg Config, path string) error {
	var err error
	if path == "" {
		if path, err = Path(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Validate rejects configurations that cannot work, with messages that say
// what to change.
func (c Config) Validate() error {
	if len(c.Input.Command) == 0 {
		return fmt.Errorf("input.command is empty: nothing can capture audio")
	}
	if len(c.Output.Command) == 0 {
		return fmt.Errorf("output.command is empty: nothing can play audio")
	}
	if c.Input.SampleRate <= 0 {
		return fmt.Errorf("input.sample_rate must be positive (16000 for whisper)")
	}
	switch c.Wake.Detector {
	case "stt", "external":
	default:
		return fmt.Errorf("wake.detector must be \"stt\" or \"external\", got %q", c.Wake.Detector)
	}
	if c.Wake.Detector == "external" && len(c.Wake.Command) == 0 {
		return fmt.Errorf("wake.detector is \"external\" but wake.command is empty")
	}
	if c.Wake.Detector == "stt" && len(c.Wake.Phrases) == 0 {
		return fmt.Errorf("wake.phrases is empty: Voice would never wake")
	}
	if c.Wake.Fuzz < 0 || c.Wake.Fuzz > 1 {
		return fmt.Errorf("wake.fuzz must be between 0 and 1, got %v", c.Wake.Fuzz)
	}
	if c.Wake.VAD.Silence.D() <= 0 {
		return fmt.Errorf("wake.vad.silence must be positive, else an utterance never ends")
	}
	if c.Wake.VAD.MaxUtterance.D() <= c.Wake.VAD.MinSpeech.D() {
		return fmt.Errorf("wake.vad.max_utterance must exceed min_speech")
	}
	switch c.Feedback.Light {
	case feedback.LightAuto, feedback.LightNone, feedback.LightMic, feedback.LightRing, feedback.LightMute, feedback.LightHold:
	default:
		return fmt.Errorf("feedback.light must be one of %s, got %q", strings.Join(feedback.Lights(), ", "), c.Feedback.Light)
	}
	if c.Feedback.Light != feedback.LightAuto && c.Feedback.Light != feedback.LightNone && c.Input.TelephonyHID == "" {
		return fmt.Errorf("feedback.light is %q but input.telephony_hid is empty: no device can show it", c.Feedback.Light)
	}
	if c.Feedback.Volume < 0 || c.Feedback.Volume > 1 {
		return fmt.Errorf("feedback.volume must be between 0 and 1, got %v", c.Feedback.Volume)
	}
	if _, err := audio.Earcon(c.Feedback.Sound, audio.Format{SampleRate: c.Input.SampleRate, Channels: 1}, c.Feedback.Volume); err != nil {
		return fmt.Errorf("feedback.sound: %w", err)
	}
	switch c.Brain.Mode {
	case "plan", "agent":
	default:
		return fmt.Errorf("brain.mode must be \"plan\" or \"agent\", got %q", c.Brain.Mode)
	}
	if len(c.Brain.Command) == 0 {
		return fmt.Errorf("brain.command is empty: nothing can reason")
	}
	seen := map[string]string{}
	for _, l := range c.Lights {
		if l.ID == "" {
			return fmt.Errorf("a light has no id")
		}
		if l.Host == "" {
			return fmt.Errorf("light %q has no host", l.ID)
		}
		k := strings.ToLower(l.ID)
		if prev, dup := seen[k]; dup {
			return fmt.Errorf("duplicate device id %q (already used by a %s)", l.ID, prev)
		}
		seen[k] = "light"
	}
	for _, t := range c.TVs {
		if t.ID == "" {
			return fmt.Errorf("a tv has no id")
		}
		if t.Adapter == "" {
			return fmt.Errorf("tv %q has no adapter (try /dev/cec0)", t.ID)
		}
		k := strings.ToLower(t.ID)
		if prev, dup := seen[k]; dup {
			return fmt.Errorf("duplicate device id %q (already used by a %s)", t.ID, prev)
		}
		seen[k] = "tv"
	}
	return nil
}
