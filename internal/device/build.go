package device

import "github.com/jerryfane/herdr-voice/internal/config"

// Build constructs the local device registry from configuration.
func Build(cfg config.Config) *Registry {
	r := NewRegistry()
	for _, l := range cfg.Lights {
		r.Add(NewMagicHome(l.ID, l.Host, l.Port, l.Protocol))
	}
	for _, t := range cfg.TVs {
		r.Add(NewCEC(t.ID, t.Adapter, t.LogicalAddress))
	}
	return r
}
