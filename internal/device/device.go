// Package device defines the contract every controllable thing implements:
// lights, televisions, and anything added later.
//
// Drivers are pure Go and speak the real local protocol of the device. Voice
// deliberately avoids cloud APIs: every driver in tree talks directly to
// hardware on the LAN or on a local bus, so the assistant keeps working with
// no internet connection.
package device

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Op is a verb a device can perform. Drivers advertise the subset they support
// via Capabilities so the planner never emits an impossible action.
type Op string

const (
	OpOn         Op = "on"
	OpOff        Op = "off"
	OpToggle     Op = "toggle"
	OpColor      Op = "color"      // args: r,g,b (0-255) or name
	OpBrightness Op = "brightness" // args: level (0-100)
	OpWhite      Op = "white"      // args: level (0-100)
	OpVolumeUp   Op = "volume_up"
	OpVolumeDown Op = "volume_down"
	OpMute       Op = "mute"
	OpInput      Op = "input" // args: source
	OpPlay       Op = "play"  // args: optional query and type (track, album, artist, playlist)
	OpResume     Op = "resume"
	OpPause      Op = "pause"
	OpNext       Op = "next"
	OpPrevious   Op = "previous"
)

// Command is a single instruction for one device.
type Command struct {
	Op   Op             `json:"op"`
	Args map[string]any `json:"args,omitempty"`
}

// State is a driver-reported snapshot. Keys are driver-defined but SHOULD use
// "power" ("on"/"off"/"standby"), "r"/"g"/"b", "white", and "brightness" where
// they apply, so the planner can describe state uniformly.
type State map[string]any

// Device is one addressable endpoint.
type Device interface {
	// ID is the stable name used in config, CLI and voice ("bedroom").
	ID() string
	// Kind is a coarse type: "light", "tv", "phone".
	Kind() string
	// Capabilities lists supported ops.
	Capabilities() []Op
	// Apply executes a command and returns the resulting state when the driver
	// can read it back. Drivers MUST verify rather than assume where the
	// protocol permits a read.
	Apply(ctx context.Context, c Command) (State, error)
	// Read returns current state without changing anything.
	Read(ctx context.Context) (State, error)
	// Describe is a one-line identity for `voice doctor` and discovery output.
	Describe() string
}

// Info is the serialisable summary handed to the planner so it knows what
// exists and what each thing can do.
type Info struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Capabilities []string `json:"capabilities"`
	Describe     string   `json:"describe,omitempty"`
}

// Registry holds the configured devices. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	d  map[string]Device
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{d: make(map[string]Device)} }

// Add registers a device, replacing any existing entry with the same ID.
func (r *Registry) Add(dev Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.d[strings.ToLower(dev.ID())] = dev
}

// Get resolves a device by ID, case-insensitively.
func (r *Registry) Get(id string) (Device, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dev, ok := r.d[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return nil, fmt.Errorf("no device %q configured (have: %s)", id, strings.Join(r.names(), ", "))
	}
	return dev, nil
}

// List returns every device, ordered by ID for stable output.
func (r *Registry) List() []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Device, 0, len(r.d))
	for _, dev := range r.d {
		out = append(out, dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Infos returns the planner-facing summary of every device.
func (r *Registry) Infos() []Info {
	devs := r.List()
	out := make([]Info, 0, len(devs))
	for _, dev := range devs {
		caps := dev.Capabilities()
		cs := make([]string, len(caps))
		for i, c := range caps {
			cs[i] = string(c)
		}
		out = append(out, Info{ID: dev.ID(), Kind: dev.Kind(), Capabilities: cs, Describe: dev.Describe()})
	}
	return out
}

// names returns sorted IDs; caller must hold at least a read lock.
func (r *Registry) names() []string {
	out := make([]string, 0, len(r.d))
	for k := range r.d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Supports reports whether dev advertises op.
func Supports(dev Device, op Op) bool {
	for _, c := range dev.Capabilities() {
		if c == op {
			return true
		}
	}
	return false
}
