package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
)

func discoverDevices(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 1 || args[0] != "home-assistant" {
		return errors.New("usage: voice discover home-assistant (explicit configured endpoint only; no LAN scan)")
	}
	if cfg.HomeAssistant == nil {
		return errors.New("configure home_assistant.url and home_assistant.token_env before discovery; no endpoint is guessed")
	}
	candidates, err := device.DiscoverHomeAssistant(ctx, *cfg.HomeAssistant)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(candidates, "", "  ")
	if err != nil {
		return errors.New("cannot encode Home Assistant discovery results")
	}
	stdout.println(string(data))
	stderr.println("Authenticated read-only discovery. Nothing imported or controlled. To approve: add an entity with your chosen id, exact entity_id and explicit operations to home_assistant.entities; empty operations is read-only. Only approve switches connected to safe loads. Locks are status-only. No direct Aqara/no-hub or non-smart thermostat support. Other domains, vendor-cloud appliances, area discovery and mDNS/SSDP are not supported by this command.")
	return nil
}
