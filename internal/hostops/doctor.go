package hostops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/media"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/secrets"
)

// Check is one line of `hermitator doctor`. A check with Note set is
// informational and cannot fail.
type Check struct {
	Name string
	Err  error
	Hint string
	Note string
}

// Failed reports whether any check failed.
func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Err != nil {
			return true
		}
	}
	return false
}

// Doctor checks that this host can run agents. The CLI prints the result;
// /host doctor sends it to the chat.
func Doctor(ctx context.Context, cfg *config.Config) []Check {
	var out []Check
	check := func(name string, err error, hint string) {
		c := Check{Name: name, Err: err}
		if err != nil {
			c.Hint = hint
		}
		out = append(out, c)
	}
	note := func(name, detail string) { out = append(out, Check{Name: name, Note: detail}) }

	if cfg.MasterKey == "" {
		check("master key", errors.New("HERMITATOR_MASTER_KEY is not set"), "run `hermitator keygen` and put the key in the service environment")
	} else {
		_, err := secrets.ParseKey(cfg.MasterKey)
		check("master key", err, "")
	}
	_, err := os.Stat(cfg.Guest)
	check("guest binary "+cfg.Guest, err, "install hermitator-guest next to hermitator, or set HERMITATOR_GUEST")

	if cfg.Runtime == "runsc" {
		r := &sandbox.Runsc{Binary: cfg.Runsc}
		check("runsc", r.Check(ctx), "install gVisor: see deploy/install.sh")
		check("overlayfs", hasFilesystem("overlay"), "load the overlay kernel module")
		for _, tool := range []string{"ip", "bridge", "nft", "mount"} {
			_, err := exec.LookPath(tool)
			check(tool, err, "install iproute2 and nftables")
		}
		if os.Geteuid() != 0 {
			check("root", errors.New("not running as root"), "hermitator needs root to mount filesystems and manage network namespaces")
		}
	} else {
		note("runtime", "unsafe-local (no isolation)")
	}

	images := &sandbox.Images{Dir: cfg.ImagesDir()}
	if fams := images.Families(); len(fams) == 0 {
		check("base images", fmt.Errorf("none in %s", cfg.ImagesDir()), "build one with deploy/build-image.sh")
	} else {
		for _, f := range fams {
			cur, _ := images.Current(f)
			_, err := images.Resolve(cur)
			check("image "+f+" → "+cur, err, "")
		}
	}

	w := &media.Whisper{Binary: cfg.Whisper, Model: cfg.Model}
	if w.Available() {
		check("voice transcription ("+filepath.Base(cfg.Model)+")", nil, "")
	} else {
		note("voice transcription", "disabled; install whisper-cli and a model to enable it")
	}
	return out
}

func hasFilesystem(name string) error {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(strings.TrimPrefix(line, "nodev")) == name {
			return nil
		}
	}
	return fmt.Errorf("%s not listed in /proc/filesystems", name)
}
