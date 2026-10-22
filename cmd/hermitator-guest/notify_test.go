package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 10}
	b.Write([]byte("0123456789"))
	b.Write([]byte("abc"))
	if got := b.String(); got != "3456789abc" {
		t.Fatalf("tail = %q", got)
	}
	// A cut through a multi-byte character leaves no broken half.
	b = &tailBuffer{max: 4}
	b.Write([]byte("xé€"))
	if got := b.String(); !strings.HasSuffix(got, "€") || strings.ContainsRune(got, '�') {
		t.Fatalf("tail = %q", got)
	}
}

// Requests carry the turn they came from, so the host answers in its chat
// even when a background job asks after another chat has spoken.
func TestRequestsCarryTheirTurn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERMITATOR_SHARED", dir)
	t.Setenv(turn.EnvTurn, "turn-42")
	askOr(turn.HostRequest{Op: "wake", Name: "tests"}, time.Millisecond, "")
	matches, _ := filepath.Glob(filepath.Join(dir, turn.RequestsDir, "*.json"))
	if len(matches) != 1 {
		t.Fatalf("requests: %v", matches)
	}
	b, _ := os.ReadFile(matches[0])
	var req turn.HostRequest
	if json.Unmarshal(b, &req) != nil || req.Turn != "turn-42" || req.Name != "tests" {
		t.Fatalf("request = %s", b)
	}
}
