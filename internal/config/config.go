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
	// Preview is the domain preview links live under, PORT-AGENT.Preview;
	// empty turns them off. HERMITATOR_PREVIEW_DOMAIN
	Preview   string
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

// DefaultSubnet is the agents' network unless HERMITATOR_SUBNET says
// otherwise.
const DefaultSubnet = "10.231.0.0/16"

// DefaultEnvFile is where deploy/install.sh writes the configuration.
const DefaultEnvFile = "/etc/hermitator/hermitator.env"

// loadEnvFile sets variables from a KEY=VALUE file without overriding the
// real environment, so commands like `hermitator user add` run on the
// server pick up the service's configuration.
func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		if !ok || !strings.HasPrefix(k, "HERMITATOR_") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}

// Load reads the configuration from the environment, falling back to
// $HERMITATOR_ENV_FILE (default DefaultEnvFile) for anything unset.
func Load() (*Config, error) {
	loadEnvFile(env("HERMITATOR_ENV_FILE", DefaultEnvFile))
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
	// Paths derived from the data directory end up as mount sources and
	// as commands run from other working directories.
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.DataDir = abs
	c.PublicURL = strings.TrimRight(env("HERMITATOR_PUBLIC_URL", "http://"+c.Listen), "/")
	c.Preview = strings.Trim(strings.ToLower(env("HERMITATOR_PREVIEW_DOMAIN", "")), ".")
	c.Model = env("HERMITATOR_WHISPER_MODEL", filepath.Join(c.DataDir, "whisper", "ggml-small.bin"))
	// Not 10.88.0.0/16, which is podman's default network, nor any other
	// range container tools or VPNs commonly claim.
	subnet, err := netip.ParsePrefix(env("HERMITATOR_SUBNET", DefaultSubnet))
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
