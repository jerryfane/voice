package config

import (
	"strings"
	"testing"
	"time"
)

func TestShippedProposalDefaultsRecordAndRateLimit(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := c.Proposals
	if !p.Enabled {
		t.Fatalf("proposals are off by default: a request Voice cannot serve would be forgotten again")
	}
	if p.MaxPerHour != 4 {
		t.Fatalf("default max_per_hour = %d, want 4", p.MaxPerHour)
	}
	if got, want := p.RetryAfter.D(), 6*time.Hour; got != want {
		t.Fatalf("default retry_after = %s, want %s", got, want)
	}
	if got, want := p.Expire.D(), 72*time.Hour; got != want {
		t.Fatalf("default expire = %s, want %s", got, want)
	}
	if got, want := p.Repo, "jerryfane/voice"; got != want {
		t.Fatalf("default repo = %q, want %q", got, want)
	}
	if p.File != "" {
		t.Fatalf("default file = %q, want empty so the state directory decides", p.File)
	}
}

func TestUnattendedApproverStillValidates(t *testing.T) {
	c := Default()
	if c.Proposals.Approver != "" {
		t.Fatalf("default approver = %q, want empty until the installer finds the owner's seat", c.Proposals.Approver)
	}
	c.Proposals.Approver = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty approver must not block startup: %v", err)
	}
}

func TestUnusableProposalSettingsAreRefusedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no notices at all", func(c *Config) { c.Proposals.MaxPerHour = 0 }, "max_per_hour"},
		{"negative notices", func(c *Config) { c.Proposals.MaxPerHour = -1 }, "max_per_hour"},
		{"no retry interval", func(c *Config) { c.Proposals.RetryAfter = 0 }, "retry_after"},
		{"no expiry", func(c *Config) { c.Proposals.Expire = 0 }, "expire"},
		{"expiry beats the second ask", func(c *Config) {
			c.Proposals.RetryAfter = Duration(6 * time.Hour)
			c.Proposals.Expire = Duration(time.Hour)
		}, "expire"},
		{"expiry equals the retry interval", func(c *Config) {
			c.Proposals.RetryAfter = Duration(6 * time.Hour)
			c.Proposals.Expire = Duration(6 * time.Hour)
		}, "expire"},
		{"repo without an owner", func(c *Config) { c.Proposals.Repo = "voice" }, "owner/name"},
		{"repo with an empty owner", func(c *Config) { c.Proposals.Repo = "/voice" }, "owner/name"},
		{"repo with an empty name", func(c *Config) { c.Proposals.Repo = "jerryfane/" }, "owner/name"},
		{"repo pasted as a URL", func(c *Config) { c.Proposals.Repo = "https://github.com/jerryfane/voice" }, "owner/name"},
		{"repo missing entirely", func(c *Config) { c.Proposals.Repo = "" }, "owner/name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.edit(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("configuration accepted, want rejection mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestDisabledProposalsIgnoreTheRestOfTheSection(t *testing.T) {
	c := Default()
	c.Proposals = Proposals{Enabled: false}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled proposals must not be validated: %v", err)
	}
	c.Proposals = Proposals{Enabled: false, MaxPerHour: -3, RetryAfter: Duration(-time.Hour), Expire: 0, Repo: "nonsense"}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled proposals must not be validated: %v", err)
	}
}
