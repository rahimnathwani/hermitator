package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/guestfiles"
	"github.com/rahimnathwani/hermitator/internal/sandbox"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

// podmanBin is replaced in tests.
var podmanBin = "podman"

// checkImageCmd runs `hermitator-guest selftest` in a podman image that
// deploy/build-image.sh has just built, before it exports it and points
// new agents at it. The container has no network, so nothing it does
// leaves the host, and it gets the same tools bundle a sandbox would.
func checkImageCmd(args []string) error {
	if len(args) != 1 || args[0] == "" || args[0][0] == '-' {
		return errors.New("usage: hermitator check-image TAG")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return checkImage(ctx, args[0], cfg.Guest)
}

func checkImage(ctx context.Context, tag, guest string) error {
	dir, err := os.MkdirTemp("", "hermitator-image-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tools, err := sandbox.InstallTools(dir, guest, guestfiles.FS)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, podmanBin, checkImageArgs(tag, tools)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New(tag + " failed its check")
	}
	return nil
}

// self is this binary's path, for scripts that call back into it.
func self() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

func checkImageArgs(tag, tools string) []string {
	return []string{"run", "--rm",
		"--network=none",
		"--pull=never",
		// A label would have to be written onto the tools directory for
		// SELinux to allow the mount; the container is ours and brief.
		"--security-opt", "label=disable",
		"--volume", tools + ":" + turn.ToolsDir + ":ro",
		tag,
		turn.ToolsDir + "/bin/hermitator-guest", "selftest",
	}
}
