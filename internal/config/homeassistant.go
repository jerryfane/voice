package config

import (
	"fmt"
	"net/url"
	"strings"
)

// HomeAssistant connects to one owner-selected server. Tokens stay in the
// service environment, and discovering an entity never authorizes its control.
type HomeAssistant struct {
	URL      string                `json:"url"`
	TokenEnv string                `json:"token_env,omitempty"`
	Entities []HomeAssistantEntity `json:"entities"`
}

type HomeAssistantEntity struct {
	ID         string   `json:"id"`
	EntityID   string   `json:"entity_id"`
	Operations []string `json:"operations,omitempty"`
}

func (h HomeAssistant) validate(seen map[string]string) error {
	u, err := url.Parse(h.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return fmt.Errorf("home_assistant.url must be an explicit HTTP(S) server origin without credentials, path, query, or fragment")
	}
	if h.TokenEnv != "" && !validFrontKeyEnv(h.TokenEnv) {
		return fmt.Errorf("home_assistant.token_env must name an uppercase environment variable, not contain a token")
	}
	entities := make(map[string]bool)
	for _, e := range h.Entities {
		id := strings.TrimSpace(e.ID)
		if id == "" || id != e.ID {
			return fmt.Errorf("home_assistant entity id must be nonempty without surrounding whitespace")
		}
		key := strings.ToLower(id)
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("duplicate device id %q (already used by a %s)", id, previous)
		}
		seen[key] = "home_assistant entity"
		domain, name, found := strings.Cut(e.EntityID, ".")
		if !found || name == "" || strings.IndexFunc(name, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) >= 0 {
			return fmt.Errorf("home_assistant entity %q has invalid entity_id", id)
		}
		var allowed string
		switch domain {
		case "light":
			allowed = " read on off toggle brightness color "
		case "switch":
			allowed = " read on off toggle "
		case "climate":
			allowed = " read temperature hvac_mode "
		case "lock":
			allowed = " read "
		default:
			return fmt.Errorf("home_assistant entity %q uses unsupported domain %q", id, domain)
		}
		if entities[e.EntityID] {
			return fmt.Errorf("home_assistant entity_id %q is configured more than once", e.EntityID)
		}
		entities[e.EntityID] = true
		ops := make(map[string]bool)
		for _, op := range e.Operations {
			if op == "" || strings.ContainsAny(op, " \t\r\n") || !strings.Contains(allowed, " "+op+" ") || ops[op] {
				return fmt.Errorf("home_assistant entity %q has unsupported or repeated operation %q", id, op)
			}
			ops[op] = true
		}
	}
	return nil
}
