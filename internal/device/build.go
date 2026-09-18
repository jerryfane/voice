package device

import "github.com/jerryfane/voice/internal/config"

// Build constructs the local device registry from configuration.
func Build(cfg config.Config) *Registry {
	r := NewRegistry()
	for _, l := range cfg.Lights {
		r.Add(NewMagicHome(l.ID, l.Host, l.Port, l.Protocol))
	}
	for _, t := range cfg.TVs {
		r.Add(NewCEC(t.ID, t.Adapter, t.LogicalAddress))
	}
	if cfg.Spotify != nil {
		r.Add(NewSpotify(cfg.Spotify.ID, cfg.Spotify.Device, cfg.Spotify.Command))
	}
	return r
}
