// Package config reads hermitator's settings from the environment.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// Config is hermitator's runtime configuration. Every field comes from a
// HERMITATOR_* environment variable; see .env.example.
type Config struct {
	DataDir   string // HERMITATOR_DATA_DIR
	Listen    string // HERMITATOR_LISTEN
	PublicURL string // HERMITATOR_PUBLIC_URL
	MasterKey string // HERMITATOR_MASTER_KEY
	Runtime   string // HERMITATOR_RUNTIME: runsc or unsafe-local
	Runsc     string // HERMITATOR_RUNSC
	Platform  string // HERMITATOR_RUNSC_PLATFORM
	RunscRoot string // HERMITATOR_RUNSC_ROOT
	Guest     string // HERMITATOR_GUEST
	Bridge    string // HERMITATOR_BRIDGE
	Subnet    netip.Prefix
	Whisper   string // HERMITATOR_WHISPER
	Model     string // HERMITATOR_WHISPER_MODEL
	LogLevel  string // HERMITATOR_LOG
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// Load reads the configuration.
func Load() (*Config, error) {
	c := &Config{
		DataDir:   env("HERMITATOR_DATA_DIR", "/var/lib/hermitator"),
		Listen:    env("HERMITATOR_LISTEN", "127.0.0.1:8080"),
		MasterKey: env("HERMITATOR_MASTER_KEY", ""),
		Runtime:   env("HERMITATOR_RUNTIME", "runsc"),
		Runsc:     env("HERMITATOR_RUNSC", "runsc"),
		Platform:  env("HERMITATOR_RUNSC_PLATFORM", "systrap"),
		RunscRoot: env("HERMITATOR_RUNSC_ROOT", "/run/hermitator/runsc"),
		Bridge:    env("HERMITATOR_BRIDGE", "hermitator0"),
		Whisper:   env("HERMITATOR_WHISPER", "whisper-cli"),
		LogLevel:  env("HERMITATOR_LOG", "info"),
	}
	c.PublicURL = strings.TrimRight(env("HERMITATOR_PUBLIC_URL", "http://"+c.Listen), "/")
	c.Model = env("HERMITATOR_WHISPER_MODEL", filepath.Join(c.DataDir, "whisper", "ggml-small.bin"))
	subnet, err := netip.ParsePrefix(env("HERMITATOR_SUBNET", "10.88.0.0/16"))
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() > 28 {
		return nil, fmt.Errorf("HERMITATOR_SUBNET must be an IPv4 prefix of /28 or larger")
	}
	c.Subnet = subnet.Masked()
	c.Guest = env("HERMITATOR_GUEST", "")
	if c.Guest == "" {
		self, err := os.Executable()
		if err == nil {
			c.Guest = filepath.Join(filepath.Dir(self), "hermitator-guest")
		}
	}
	switch c.Runtime {
	case "runsc", "unsafe-local":
	default:
		return nil, fmt.Errorf("HERMITATOR_RUNTIME must be runsc or unsafe-local, not %q", c.Runtime)
	}
	return c, nil
}

// Paths inside DataDir.
func (c *Config) DBPath() string     { return filepath.Join(c.DataDir, "hermitator.db") }
func (c *Config) WhatsAppDB() string { return filepath.Join(c.DataDir, "whatsapp.db") }
func (c *Config) AgentsDir() string  { return filepath.Join(c.DataDir, "agents") }
func (c *Config) ImagesDir() string  { return filepath.Join(c.DataDir, "images") }
func (c *Config) ToolsDir() string   { return filepath.Join(c.DataDir, "tools") }
