package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// command runs a host command and folds its output into the error.
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// commandInput is command with data on stdin.
func commandInput(ctx context.Context, input string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
