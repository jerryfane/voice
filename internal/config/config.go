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
	"net"
	"net/url"
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
	// STT configures local and optional remote speech-to-text engines.
	STT STT `json:"stt"`
	// TTS is text-to-speech.
	TTS Engine `json:"tts"`
	// Brain is the reasoning layer.
	Brain Brain `json:"brain"`
	// Lights are Zengge/Magic Home controllers (local protocol, port 5577).
	Lights []Light `json:"lights"`
	// TVs are HDMI-CEC displays.
	TVs []TV `json:"tvs"`
	// Spotify is an optional local spotify_player playback daemon.
	Spotify *Spotify `json:"spotify,omitempty"`
}

// Feedback configures the immediate acknowledgement of an accepted wake
// phrase. It is emitted by the session layer, so it works with no network and
// no model.
type Feedback struct {
	// Thinking is the sound played on a loop while the planner works: "tick",
	// "hum" or "none". A request that leaves the device takes seconds, and
	// silence during that wait is indistinguishable from Voice having missed
	// the question. It starts only after the answer has already taken longer
	// than a moment, so a locally answered query stays silent.
	//
	// ThinkingVolume scales it independently of Volume, between 0 (silent)
	// and 1 (full scale). It defaults to a third of Volume, which is where it
	// was fixed before: the sound plays underneath someone waiting rather
	// than at them. Set it explicitly to override that.
	//
	// It is its own field because the fixed ratio was asked about twice - by
	// the reviewer, and by the owner speaking to the device, which answered
	// "I can't adjust the thinking sound volume from the controls available
	// to me." Two requests for the same knob is enough.
	Thinking string `json:"thinking"`
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
	// ThinkingVolume scales the thinking sound, between 0 (silent) and 1.
	//
	// A POINTER so absence and zero are different things. With a plain
	// float64, an explicit thinking_volume of 0 is indistinguishable from an
	// omitted field, so the one value a user most obviously wants - silence,
	// from the very setting added to control it - would have been read as
	// "derive it from Volume" and quietly ignored.
	ThinkingVolume *float64 `json:"thinking_volume,omitempty"`
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
	// Fuzz is the maximum normalised distance (0-1) still counted as a match,
	// measured on the SOUND skeleton rather than on spelling. 0 demands the
	// transcript sound identical; the default 0.2 tolerates one class in five.
	//
	// It defaults to 0.2 rather than 0 because 0 made the feature unusable on
	// real hardware: whisper rendered the configured "hey voice" as "hey
	// boys", so the exact-match default never woke Voice while the microphone
	// and the transcription were both working. At 0.2 that transcript matches
	// and ordinary speech - including "hey buzz light year", which collides
	// once vowels are discarded - still does not.
	Fuzz float64 `json:"fuzz"`
	// Detector selects the strategy: "sherpa" detects a configured phrase in
	// streaming audio; "stt" keeps the slower whole-utterance Whisper gate.
	Detector string      `json:"detector"`
	Sherpa   *SherpaWake `json:"sherpa,omitempty"`
	// VAD tunes speech segmentation.
	VAD VAD `json:"vad"`
}

// SherpaWake configures the resident open-vocabulary keyword spotter.
type SherpaWake struct {
	Encoder           string  `json:"encoder"`
	Decoder           string  `json:"decoder"`
	Joiner            string  `json:"joiner"`
	Tokens            string  `json:"tokens"`
	Keywords          string  `json:"keywords"`
	NumThreads        int     `json:"num_threads"`
	MaxActivePaths    int     `json:"max_active_paths"`
	KeywordsScore     float64 `json:"keywords_score"`
	KeywordsThreshold float64 `json:"keywords_threshold"`
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

// STT keeps the existing command engine as the final fallback and optionally
// puts a resident local server and OpenRouter in front of it.
type STT struct {
	Engine
	Resident   *ResidentSTT   `json:"resident,omitempty"`
	OpenRouter *OpenRouterSTT `json:"openrouter,omitempty"`
}

// ResidentSTT describes a loopback-only whisper.cpp server.
type ResidentSTT struct {
	Endpoint string   `json:"endpoint"`
	Timeout  Duration `json:"timeout"`
}

// OpenRouterSTT describes the cloud transcription endpoint. APIKeyEnv names
// the environment variable; the secret itself never belongs in config.
type OpenRouterSTT struct {
	Endpoint  string   `json:"endpoint"`
	Model     string   `json:"model"`
	APIKeyEnv string   `json:"api_key_env"`
	Timeout   Duration `json:"timeout"`
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
	// Rules enables the local no-model fast path for trivial queries: the
	// clock, the date, stop and cancel, and device commands the rules
	// recognise. It defaults to TRUE because shipping it false meant the
	// device asked a remote model what time it was - 7.1 seconds measured on
	// the owner's hardware - while holding the answer in its own clock. Set
	// it false to send every utterance to the planner.
	Rules bool `json:"rules"`
	// Jev optionally routes bounded device actions before the agent.
	Jev *Jev `json:"jev,omitempty"`
}

// Jev configures one OpenRouter Decisions request. APIKeyEnv names the
// protected environment variable rather than storing its value.
type Jev struct {
	Endpoint   string   `json:"endpoint"`
	Model      string   `json:"model"`
	APIKeyEnv  string   `json:"api_key_env"`
	Confidence float64  `json:"confidence"`
	Timeout    Duration `json:"timeout"`
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

// Spotify describes the local playback endpoint exposed by spotify_player.
type Spotify struct {
	// ID is what the planner calls this music device.
	ID string `json:"id"`
	// Device is the Spotify Connect endpoint name exposed by the daemon.
	Device string `json:"device,omitempty"`
	// Command is the spotify_player CLI used to reach its loopback daemon.
	Command Command `json:"command"`
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
			Phrases:  []string{"hey voice", "hey boys"},
			Fuzz:     0.2,
			Detector: "sherpa",
			Sherpa: &SherpaWake{
				Encoder:           "/var/lib/voice/models/sherpa-kws/encoder.int8.onnx",
				Decoder:           "/var/lib/voice/models/sherpa-kws/decoder.onnx",
				Joiner:            "/var/lib/voice/models/sherpa-kws/joiner.int8.onnx",
				Tokens:            "/var/lib/voice/models/sherpa-kws/tokens.txt",
				Keywords:          "/var/lib/voice/models/sherpa-kws/keywords.txt",
				NumThreads:        1,
				MaxActivePaths:    4,
				KeywordsScore:     4,
				KeywordsThreshold: 0.15,
			},
			VAD: VAD{
				Threshold:    0,
				MinSpeech:    Duration(250 * time.Millisecond),
				Silence:      Duration(700 * time.Millisecond),
				MaxUtterance: Duration(6 * time.Second),
				PreRoll:      Duration(1500 * time.Millisecond),
			},
		},
		Feedback: Feedback{
			Light:    "auto",
			Sound:    audio.EarconChime,
			Thinking: audio.ThinkingTick,
			Volume:   0.35,
		},
		Timers: Timers{Enabled: true},
		STT: STT{
			Engine: Engine{
				Name:    "whisper.cpp",
				Model:   "models/ggml-base.en.bin",
				Timeout: Duration(30 * time.Second),
				Command: Command{
					"whisper-cli", "-m", "{model}", "-f", "{file}",
					"--no-timestamps", "--no-prints", "-t", "3", "-l", "en",
					"--prompt", "Hey Voice.",
				},
			},
			Resident: &ResidentSTT{
				Endpoint: "http://127.0.0.1:8178/inference",
				Timeout:  Duration(10 * time.Second),
			},
			OpenRouter: &OpenRouterSTT{
				Endpoint:  "https://openrouter.ai/api/v1/audio/transcriptions",
				Model:     "openai/whisper-large-v3-turbo",
				APIKeyEnv: "OPENROUTER_API_KEY",
				Timeout:   Duration(5 * time.Second),
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
			Rules:   true,
			Jev: &Jev{
				Endpoint:   "https://openrouter.ai/api/alpha/decisions",
				Model:      "typesafe/jev-1.13",
				APIKeyEnv:  "OPENROUTER_API_KEY",
				Confidence: 0.85,
				Timeout:    Duration(2 * time.Second),
			},
		},
		Lights: []Light{},
		TVs:    []TV{},
	}
}

// SystemPath is where an installed device keeps its config. The service unit
// passes it explicitly; every other command has to find it.
const SystemPath = "/etc/voice/config.json"

// systemPath is the location actually consulted, as a variable so the
// fallback can be tested without a real /etc/voice on the host - otherwise
// the test for "fall back to the installed config" only runs on a device,
// which is a check that cannot fail anywhere else. Production never reassigns
// it.
var systemPath = SystemPath

// NotFoundError says where a config was looked for, because the useful part
// of "no config" is which files were considered.
type NotFoundError struct {
	Looked []string
	// UserPath is the per-user file `voice init` writes by default, empty if
	// it could not be resolved at all - HOME unset, for instance. The system
	// path was still checked in that case, so the error must still name it
	// rather than degrading to a bare "cannot locate home directory".
	UserPath string
	// System is the installed location, named separately because on a device
	// it is the file the operator almost certainly wants to create.
	System string
}

func (e *NotFoundError) Error() string {
	msg := "no config found; looked in " + strings.Join(e.Looked, " and ") + "."
	switch {
	case e.UserPath != "":
		msg += fmt.Sprintf(" Run `voice init` to write %s, `voice init --config %s` for a device install, or pass --config PATH",
			e.UserPath, e.System)
	default:
		msg += fmt.Sprintf(" Run `voice init --config %s` to write it, or pass --config PATH", e.System)
	}
	return msg
}

// Locate finds the config to READ, preferring the caller's own over the
// installed one.
//
// The per-user path alone was not enough: an installed device keeps its
// config at SystemPath and the service passes it explicitly, so every command
// a person would type to inspect that device failed - and the failure advised
// `voice init`, which writes a SECOND config into the caller's home that the
// service never reads. Somebody following the program's own instruction would
// tune a file with no effect on the running assistant and conclude it worked:
// a machine teaching its operator a false model of itself.
func Locate() (string, error) {
	if p := os.Getenv("VOICE_CONFIG"); p != "" {
		return p, nil
	}
	looked := make([]string, 0, 2)
	own, ownErr := Path()
	if ownErr == nil {
		looked = append(looked, own)
		if _, err := os.Stat(own); err == nil {
			return own, nil
		}
	}
	looked = append(looked, systemPath)
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath, nil
	}
	// Even when the per-user path could not be resolved, the installed one WAS
	// checked, so the error names it. Returning the raw home-directory error
	// instead would hide the file that actually matters on a device.
	if ownErr != nil {
		own = ""
	}
	return "", &NotFoundError{Looked: looked, UserPath: own, System: systemPath}
}

// Path returns the per-user config location - the file `voice init` writes -
// honouring VOICE_CONFIG and XDG_CONFIG_HOME before falling back to
// ~/.config/voice/config.json. Use Locate to decide which file to READ.
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
		if path, err = Locate(); err != nil {
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

// ResolvedThinkingVolume is the level the thinking sound actually plays at:
// the explicit setting when given, otherwise a share of Volume. One function
// answers it so the renderer, the validator and `voice doctor` cannot
// disagree — two places deriving the same number is how a report drifts from
// the behaviour it describes, which this repo has already been caught doing.
func (f Feedback) ResolvedThinkingVolume() float64 {
	if f.ThinkingVolume != nil {
		return *f.ThinkingVolume
	}
	return f.Volume * audio.ThinkingVolumeRatio
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
	if len(c.Wake.Phrases) == 0 {
		return fmt.Errorf("wake.phrases is empty: Voice would never wake")
	}
	switch c.Wake.Detector {
	case "stt":
	case "sherpa":
		if c.Wake.Sherpa == nil {
			return fmt.Errorf("wake.sherpa is required when wake.detector is \"sherpa\"")
		}
		s := c.Wake.Sherpa
		for _, required := range []struct {
			label string
			path  string
		}{
			{"encoder", s.Encoder},
			{"decoder", s.Decoder},
			{"joiner", s.Joiner},
			{"tokens", s.Tokens},
			{"keywords", s.Keywords},
		} {
			if strings.TrimSpace(required.path) == "" {
				return fmt.Errorf("wake.sherpa.%s is empty", required.label)
			}
		}
		if s.NumThreads <= 0 || s.MaxActivePaths <= 0 {
			return fmt.Errorf("wake.sherpa thread and path counts must be positive")
		}
		if s.KeywordsScore <= 0 {
			return fmt.Errorf("wake.sherpa.keywords_score must be positive")
		}
		if s.KeywordsThreshold <= 0 || s.KeywordsThreshold >= 1 {
			return fmt.Errorf("wake.sherpa.keywords_threshold must be between 0 and 1")
		}
	default:
		return fmt.Errorf("wake.detector must be \"sherpa\" or \"stt\", got %q", c.Wake.Detector)
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
	if c.STT.Resident != nil {
		if strings.TrimSpace(c.STT.Resident.Endpoint) == "" {
			return fmt.Errorf("stt.resident.endpoint is empty")
		}
		u, err := url.Parse(c.STT.Resident.Endpoint)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" {
			return fmt.Errorf("stt.resident.endpoint must be an HTTP loopback URL")
		}
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("stt.resident.endpoint must use localhost or a loopback address")
		}
		if c.STT.Resident.Timeout.D() <= 0 {
			return fmt.Errorf("stt.resident.timeout must be positive")
		}
	}
	if c.STT.OpenRouter != nil {
		if strings.TrimSpace(c.STT.OpenRouter.Endpoint) == "" {
			return fmt.Errorf("stt.openrouter.endpoint is empty")
		}
		u, err := url.Parse(c.STT.OpenRouter.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("stt.openrouter.endpoint must be an HTTPS URL")
		}
		if strings.TrimSpace(c.STT.OpenRouter.Model) == "" {
			return fmt.Errorf("stt.openrouter.model is empty")
		}
		if strings.TrimSpace(c.STT.OpenRouter.APIKeyEnv) == "" {
			return fmt.Errorf("stt.openrouter.api_key_env is empty")
		}
		if c.STT.OpenRouter.Timeout.D() <= 0 {
			return fmt.Errorf("stt.openrouter.timeout must be positive")
		}
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
	// Validated here for the same reason as the acknowledgement: a typo must
	// be reported at startup, not discovered as silence while someone waits
	// for an answer.
	if v := c.Feedback.ThinkingVolume; v != nil && (*v < 0 || *v > 1) {
		return fmt.Errorf("feedback.thinking_volume must be between 0 and 1, got %v", *v)
	}
	if _, err := audio.Thinking(c.Feedback.Thinking, audio.Format{SampleRate: c.Input.SampleRate, Channels: 1}, c.Feedback.ResolvedThinkingVolume()); err != nil {
		return fmt.Errorf("feedback.thinking: %w", err)
	}
	switch c.Brain.Mode {
	case "plan", "agent":
	default:
		return fmt.Errorf("brain.mode must be \"plan\" or \"agent\", got %q", c.Brain.Mode)
	}
	if len(c.Brain.Command) == 0 {
		return fmt.Errorf("brain.command is empty: nothing can reason")
	}
	if c.Brain.Jev != nil {
		if strings.TrimSpace(c.Brain.Jev.Endpoint) == "" {
			return fmt.Errorf("brain.jev.endpoint is empty")
		}
		u, err := url.Parse(c.Brain.Jev.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("brain.jev.endpoint must be an HTTPS URL")
		}
		if strings.TrimSpace(c.Brain.Jev.Model) == "" {
			return fmt.Errorf("brain.jev.model is empty")
		}
		if strings.TrimSpace(c.Brain.Jev.APIKeyEnv) == "" {
			return fmt.Errorf("brain.jev.api_key_env is empty")
		}
		if c.Brain.Jev.Confidence <= 0 || c.Brain.Jev.Confidence > 1 {
			return fmt.Errorf("brain.jev.confidence must be above 0 and at most 1")
		}
		if c.Brain.Jev.Timeout.D() <= 0 {
			return fmt.Errorf("brain.jev.timeout must be positive")
		}
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
	if c.Spotify != nil {
		if c.Spotify.ID == "" {
			return fmt.Errorf("spotify has no id")
		}
		if len(c.Spotify.Command) == 0 {
			return fmt.Errorf("spotify %q has no command", c.Spotify.ID)
		}
		k := strings.ToLower(c.Spotify.ID)
		if prev, dup := seen[k]; dup {
			return fmt.Errorf("duplicate device id %q (already used by a %s)", c.Spotify.ID, prev)
		}
	}
	return nil
}
