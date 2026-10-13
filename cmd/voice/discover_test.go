package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/device"
	"github.com/jerryfane/voice/internal/session"
)

func TestDiscoverRequiresExplicitEndpointAndDoesNotImport(t *testing.T) {
	var out, notes bytes.Buffer
	savedOut, savedErr := stdout, stderr
	stdout, stderr = &printer{w: &out}, &printer{w: &notes}
	defer func() { stdout, stderr = savedOut, savedErr }()
	if err := discoverDevices(context.Background(), config.Config{}, []string{"home-assistant"}); err == nil {
		t.Fatal("discovery guessed an endpoint")
	}
	if err := discoverDevices(context.Background(), config.Config{}, []string{"192.168.1.0/24"}); err == nil {
		t.Fatal("discovery accepted a subnet")
	}
	t.Setenv("VOICE_HA_CLI_TEST_TOKEN", "private-test-token")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Errorf("unexpected method or missing auth")
		}
		var value any = map[string]string{"message": "API running."}
		if r.URL.Path == "/api/states" {
			value = []map[string]any{{"entity_id": "light.test", "state": "off", "attributes": map[string]string{"private": "private-test-token"}}}
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close() // discard: httptest.Server.Close has no return value.
	cfg := config.Config{HomeAssistant: &config.HomeAssistant{URL: srv.URL, TokenEnv: "VOICE_HA_CLI_TEST_TOKEN"}}
	if err := discoverDevices(context.Background(), cfg, []string{"home-assistant"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(out.String(), "light.test") || !strings.Contains(notes.String(), "Nothing imported or controlled") {
		t.Fatalf("calls=%d out=%s notes=%s", calls, out.String(), notes.String())
	}
	if strings.Contains(out.String()+notes.String(), "private-test-token") {
		t.Fatal("credential leaked")
	}
	if len(cfg.HomeAssistant.Entities) != 0 || len(device.Build(cfg).List()) != 0 {
		t.Fatal("discovery automatically imported candidate")
	}
}

type climateCLIRecorder struct{ command device.Command }

func (d *climateCLIRecorder) ID() string   { return "hall" }
func (d *climateCLIRecorder) Kind() string { return "climate" }
func (d *climateCLIRecorder) Capabilities() []device.Op {
	return []device.Op{device.OpRead, device.OpTemperature, device.OpHVACMode}
}
func (d *climateCLIRecorder) Describe() string { return "test recorder" }
func (d *climateCLIRecorder) Read(context.Context) (device.State, error) {
	return device.State{"hvac_mode": "heat"}, nil
}
func (d *climateCLIRecorder) Apply(_ context.Context, cmd device.Command) (device.State, error) {
	d.command = cmd
	return device.State{"hvac_mode": "heat"}, nil
}

func TestClimateCLIMapsTypedArguments(t *testing.T) {
	var out bytes.Buffer
	saved := stdout
	stdout = &printer{w: &out}
	defer func() { stdout = saved }()
	d := &climateCLIRecorder{}
	registry := device.NewRegistry()
	registry.Add(d)
	a := &session.Assistant{Devices: registry}
	if err := commandDevice(context.Background(), a, "climate", []string{"hall", "temperature", "21.5"}); err != nil {
		t.Fatal(err)
	}
	if d.command.Op != device.OpTemperature || d.command.Args["temperature"] != 21.5 {
		t.Fatalf("command = %#v", d.command)
	}
	if err := commandDevice(context.Background(), a, "climate", []string{"hall", "hvac-mode", "cool"}); err != nil {
		t.Fatal(err)
	}
	if d.command.Op != device.OpHVACMode || d.command.Args["mode"] != "cool" {
		t.Fatalf("command = %#v", d.command)
	}
	if err := commandDevice(context.Background(), a, "climate", []string{"hall", "unlock"}); err == nil {
		t.Fatal("unadvertised operation passed CLI validation")
	}
}
