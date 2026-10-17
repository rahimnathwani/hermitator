package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFileFillsGapsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermitator.env")
	os.WriteFile(path, []byte("# comment\nHERMITATOR_LISTEN=0.0.0.0:9\nexport HERMITATOR_LOG=\"debug\"\nOTHER=ignored\n"), 0o600)
	t.Setenv("HERMITATOR_ENV_FILE", path)
	t.Setenv("HERMITATOR_LISTEN", "127.0.0.1:1234")
	t.Setenv("HERMITATOR_DATA_DIR", "relative/dir")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:1234" {
		t.Fatalf("env file overrode the environment: %s", c.Listen)
	}
	if c.LogLevel != "debug" {
		t.Fatalf("env file was not read: log=%s", c.LogLevel)
	}
	if !filepath.IsAbs(c.DataDir) {
		t.Fatalf("data dir %q is not absolute", c.DataDir)
	}
	if os.Getenv("OTHER") != "" {
		t.Fatal("non-hermitator variable was imported")
	}
}

func TestBadSubnet(t *testing.T) {
	t.Setenv("HERMITATOR_ENV_FILE", "/nonexistent")
	t.Setenv("HERMITATOR_SUBNET", "10.0.0.0/30")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a tiny subnet")
	}
}
