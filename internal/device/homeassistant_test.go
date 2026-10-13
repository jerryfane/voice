package device

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jerryfane/voice/internal/config"
)

const haTestToken = "test-only-never-log-this-secret"

func haTestConfig(t *testing.T, handler http.HandlerFunc) config.HomeAssistant {
	t.Helper()
	t.Setenv("VOICE_HA_TEST_TOKEN", haTestToken)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+haTestToken {
			t.Errorf("missing expected bearer authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return config.HomeAssistant{URL: srv.URL, TokenEnv: "VOICE_HA_TEST_TOKEN"}
}

func haJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestHomeAssistantDiscoveryIsAuthenticatedReadOnlyAndRedacted(t *testing.T) {
	var paths []string
	cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery wrote using %s", r.Method)
		}
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/":
			haJSON(t, w, map[string]any{"message": "API running."})
		case "/api/states":
			haJSON(t, w, []haState{
				{EntityID: "switch.fan", State: "on", Attributes: map[string]any{"latitude": 123, "token": haTestToken}},
				{EntityID: "lock.front", State: "locked"},
				{EntityID: "climate.hall", State: "heat"},
				{EntityID: "light.desk", State: "off"},
				{EntityID: "camera.private", State: "streaming"},
				{EntityID: "light.bad/../../services/lock/unlock", State: "on"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	got, err := DiscoverHomeAssistant(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].EntityID != "climate.hall" || got[3].EntityID != "switch.fan" {
		t.Fatalf("candidates = %#v", got)
	}
	if !reflect.DeepEqual(paths, []string{"/api/", "/api/states"}) {
		t.Fatalf("requests = %v", paths)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), haTestToken) || strings.Contains(string(encoded), "latitude") {
		t.Fatalf("discovery exposed private attributes")
	}
	if !strings.Contains(got[2].Policy, "status only") {
		t.Fatalf("lock policy = %q", got[2].Policy)
	}
}

func TestHomeAssistantRejectsUnsafeConfigurationAndOperationsBeforeNetwork(t *testing.T) {
	cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(500)
	})
	for _, tc := range []struct {
		entity  string
		ops     []string
		command Op
	}{
		{"lock.front", []string{"unlock"}, Op("unlock")},
		{"lock.front", []string{"lock"}, Op("lock")},
		{"lock.front", nil, Op("unlock")},
		{"light.desk", nil, OpOn},
		{"light.desk", []string{"on"}, OpOff},
		{"switch.fan", []string{"color"}, OpColor},
		{"climate.hall", []string{"on"}, OpOn},
		{"cover.garage", []string{"on"}, OpOn},
		{"light.desk,light.other", []string{"on"}, OpOn},
		{"light.desk/../../services/lock/unlock", []string{"on"}, OpOn},
	} {
		t.Run(tc.entity+string(tc.command), func(t *testing.T) {
			d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "test", EntityID: tc.entity, Operations: tc.ops})
			if _, err := d.Apply(context.Background(), Command{Op: tc.command}); err == nil {
				t.Fatal("unsafe operation succeeded")
			}
		})
	}
	for _, endpoint := range []string{"https://user:secret@localhost", "http://localhost/?token=secret", "http://localhost/#secret", "http://localhost/path", "ftp://localhost"} {
		if _, err := newHAAPI(config.HomeAssistant{URL: endpoint}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe URL handling: %v", err)
		}
	}
	if _, err := dialHALocal(context.Background(), "tcp", "203.0.113.1:8123"); err == nil {
		t.Fatal("public endpoint permitted")
	}
}

func TestHomeAssistantCredentialsAndServerErrorsAreNotExposed(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				haJSON(t, w, map[string]string{"error": haTestToken})
			})
			d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "desk", EntityID: "light.desk"})
			_, err := d.Read(context.Background())
			if err == nil || strings.Contains(err.Error(), haTestToken) || !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(d.Describe(), cfg.URL) || strings.Contains(d.Describe(), haTestToken) {
				t.Fatal("description leaked endpoint/secret")
			}
		})
	}
	t.Run("missing token", func(t *testing.T) {
		cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) { t.Error("missing token still sent a request") })
		t.Setenv(cfg.TokenEnv, "")
		if _, err := DiscoverHomeAssistant(context.Background(), cfg); err == nil {
			t.Fatal("missing token accepted")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed credential-bearing redirect") }))
		defer target.Close() // discard: httptest.Server.Close has no return value.
		cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
		if _, err := DiscoverHomeAssistant(context.Background(), cfg); err == nil || strings.Contains(err.Error(), haTestToken) {
			t.Fatalf("redirect error = %v", err)
		}
	})
}

func TestHomeAssistantApplyVerifiesSelectedEntityAfterService(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		serviceError, unchanged, delayed bool
	}{
		{name: "verified"}, {name: "eventually verified", delayed: true}, {name: "service error", serviceError: true}, {name: "acceptance is not success", unchanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			posted, readsAfterPost := false, 0
			cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method + " " + r.URL.Path {
				case "GET /api/states/switch.fan":
					state := "off"
					if posted {
						readsAfterPost++
						if !tc.unchanged && (!tc.delayed || readsAfterPost > 1) {
							state = "on"
						}
					}
					haJSON(t, w, haState{EntityID: "switch.fan", State: state})
				case "POST /api/services/switch/turn_on":
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if !reflect.DeepEqual(payload, map[string]any{"entity_id": "switch.fan"}) {
						t.Errorf("service target = %v", payload)
					}
					posted = true
					if tc.serviceError {
						w.WriteHeader(500)
						haJSON(t, w, haTestToken)
						return
					}
					// A success response claiming on must never bypass GET verification.
					haJSON(t, w, []haState{{EntityID: "switch.fan", State: "on"}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			})
			d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "fan", EntityID: "switch.fan", Operations: []string{"on"}})
			ctx := context.Background()
			if tc.unchanged {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 70*time.Millisecond)
				defer cancel()
			}
			state, err := d.Apply(ctx, Command{Op: OpOn})
			if tc.serviceError || tc.unchanged {
				if err == nil || strings.Contains(err.Error(), haTestToken) {
					t.Fatalf("unverified write succeeded/leaked: state=%v err=%v", state, err)
				}
			} else if err != nil || state["power"] != "on" {
				t.Fatalf("state=%v err=%v", state, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !posted {
				t.Error("service not invoked")
			}
			if !tc.serviceError && readsAfterPost == 0 {
				t.Error("state was not read back")
			}
		})
	}
}

func TestHomeAssistantSupportedLightAndClimateCommands(t *testing.T) {
	for _, tc := range []struct {
		entity, state, service string
		attrs                  map[string]any
		cmd                    Command
		field                  string
		want                   any
		after                  string
		updates                map[string]any
	}{
		{"light.desk", "off", "turn_on", map[string]any{"supported_color_modes": []string{"brightness"}}, Command{Op: OpBrightness, Args: map[string]any{"level": 50}}, "brightness_pct", float64(50), "on", map[string]any{"brightness": 128}},
		{"light.desk", "off", "turn_on", map[string]any{"supported_color_modes": []string{"rgb"}}, Command{Op: OpColor, Args: map[string]any{"name": "red"}}, "rgb_color", []any{float64(255), float64(0), float64(0)}, "on", map[string]any{"rgb_color": []int{255, 0, 0}}},
		{"climate.hall", "heat", "set_temperature", map[string]any{"min_temp": 10, "max_temp": 30, "supported_features": 1}, Command{Op: OpTemperature, Args: map[string]any{"temperature": 21.5}}, "temperature", 21.5, "heat", map[string]any{"temperature": 21.5}},
		{"climate.hall", "heat", "set_hvac_mode", map[string]any{"hvac_modes": []string{"off", "heat", "cool"}}, Command{Op: OpHVACMode, Args: map[string]any{"mode": "cool"}}, "hvac_mode", "cool", "cool", nil},
	} {
		t.Run(string(tc.cmd.Op), func(t *testing.T) {
			state := tc.state
			cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				domain := strings.SplitN(tc.entity, ".", 2)[0]
				if r.Method == http.MethodGet && r.URL.Path == "/api/states/"+tc.entity {
					haJSON(t, w, haState{EntityID: tc.entity, State: state, Attributes: tc.attrs})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/services/"+domain+"/"+tc.service {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["entity_id"] != tc.entity || !reflect.DeepEqual(payload[tc.field], tc.want) || len(payload) != 2 {
					t.Errorf("payload = %v", payload)
				}
				state = tc.after
				for k, v := range tc.updates {
					tc.attrs[k] = v
				}
				haJSON(t, w, []haState{})
			})
			d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "test", EntityID: tc.entity, Operations: []string{string(tc.cmd.Op)}})
			if _, err := d.Apply(context.Background(), tc.cmd); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHomeAssistantRejectsUnsupportedLiveCapabilitiesAndArguments(t *testing.T) {
	for _, tc := range []struct {
		entity, state string
		attrs         map[string]any
		cmd           Command
	}{
		{"light.desk", "on", map[string]any{"supported_color_modes": []string{"onoff"}}, Command{Op: OpBrightness, Args: map[string]any{"level": 50}}},
		{"light.desk", "on", map[string]any{"supported_color_modes": []string{"rgb"}}, Command{Op: OpBrightness, Args: map[string]any{"level": 50.2}}},
		{"light.desk", "on", map[string]any{"supported_color_modes": []string{"rgb"}}, Command{Op: OpColor, Args: map[string]any{"r": -1, "g": 0, "b": 0}}},
		{"light.desk", "on", map[string]any{"supported_color_modes": []string{"brightness"}}, Command{Op: OpColor, Args: map[string]any{"name": "red"}}},
		{"climate.hall", "heat", map[string]any{"min_temp": 10, "max_temp": 30, "supported_features": 1}, Command{Op: OpTemperature, Args: map[string]any{"temperature": 80}}},
		{"climate.hall", "heat_cool", map[string]any{"min_temp": 10, "max_temp": 30, "supported_features": 2}, Command{Op: OpTemperature, Args: map[string]any{"temperature": 20}}},
		{"climate.hall", "heat", map[string]any{"hvac_modes": []string{"heat"}}, Command{Op: OpHVACMode, Args: map[string]any{"mode": "cool"}}},
	} {
		t.Run(tc.entity+string(tc.cmd.Op), func(t *testing.T) {
			cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("invalid command reached service")
					w.WriteHeader(500)
					return
				}
				haJSON(t, w, haState{EntityID: tc.entity, State: tc.state, Attributes: tc.attrs})
			})
			d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "test", EntityID: tc.entity, Operations: []string{string(tc.cmd.Op)}})
			if _, err := d.Apply(context.Background(), tc.cmd); err == nil {
				t.Fatal("unsupported command accepted")
			}
		})
	}
}

func TestHomeAssistantLockStatusAndRegistryRemainReadOnly(t *testing.T) {
	cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/states/lock.front" {
			t.Errorf("lock write or unexpected read: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		haJSON(t, w, haState{EntityID: "lock.front", State: "locked", Attributes: map[string]any{"code": "private", "friendly_name": "private location"}})
	})
	cfg.Entities = []config.HomeAssistantEntity{{ID: "front", EntityID: "lock.front"}}
	registry := Build(config.Config{HomeAssistant: &cfg})
	d, err := registry.Get("front")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Capabilities(), []Op{OpRead}) {
		t.Fatalf("lock capabilities = %v", d.Capabilities())
	}
	got, err := d.Apply(context.Background(), Command{Op: OpRead})
	if err != nil || !reflect.DeepEqual(got, State{"state": "locked", "locked": true}) {
		t.Fatalf("state = %v, err = %v", got, err)
	}
	if _, err := d.Apply(context.Background(), Command{Op: Op("unlock")}); err == nil {
		t.Fatal("unlock succeeded")
	}
	if len(registry.List()) != 1 {
		t.Fatal("unselected entities imported")
	}
}

func TestHomeAssistantRejectsUnknownAndMismatchedState(t *testing.T) {
	for _, state := range []haState{
		{EntityID: "light.other", State: "on"},
		{EntityID: "light.desk", State: "unavailable"},
		{EntityID: "light.desk", State: "unknown"},
		{EntityID: "light.desk", State: haTestToken},
	} {
		cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) { haJSON(t, w, state) })
		d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "desk", EntityID: "light.desk"})
		if _, err := d.Read(context.Background()); err == nil || strings.Contains(err.Error(), haTestToken) {
			t.Fatalf("unexpected state accepted/leaked: %v", err)
		}
	}
}

func TestHomeAssistantReadReportsOnlyVerifiedClimateMeasurements(t *testing.T) {
	cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		haJSON(t, w, haState{EntityID: "climate.hall", State: "heat", Attributes: map[string]any{
			"temperature": 21.5, "current_temperature": 19.5, "temperature_unit": "°C",
			"friendly_name": "private location", "access_token": haTestToken,
		}})
	})
	d := NewHomeAssistant(cfg, config.HomeAssistantEntity{ID: "hall", EntityID: "climate.hall"})
	got, err := d.Read(context.Background())
	want := State{"state": "heat", "hvac_mode": "heat", "temperature": 21.5, "current_temperature": 19.5, "temperature_unit": "°C"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state=%v err=%v", got, err)
	}
}

func TestHomeAssistantDiscoveryCancellation(t *testing.T) {
	started := make(chan struct{})
	cfg := haTestConfig(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := DiscoverHomeAssistant(ctx, cfg); result <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery did not reach local server")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery did not stop on cancellation")
	}
}
