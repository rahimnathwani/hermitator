package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AgentsCgroup is the cgroup every sandbox's own cgroup sits under.
const AgentsCgroup = "hermitator"

// Limits for all sandboxes together.
const (
	// agentsCPUWeight is half the default, so under contention hermitator,
	// Caddy and anything else on the host get the CPU first.
	agentsCPUWeight = 50
	minReserve      = 1 << 30
	maxReserve      = 4 << 30
)

// LimitAll caps the memory and CPU share of all sandboxes together.
//
// Each sandbox has its own memory limit, but several busy ones can still
// exhaust the host between them, and then the kernel's OOM killer may
// choose hermitator itself, taking WhatsApp down with it. With a ceiling
// on the parent cgroup the OOM killer only ever chooses among sandboxes.
// The ceiling leaves a fifth of the host's memory, between 1 and 4 GiB,
// for everything else. root is the cgroup2 mount, normally /sys/fs/cgroup.
func LimitAll(root string, memTotal uint64) (memMax uint64, err error) {
	reserve := min(max(memTotal/5, minReserve), maxReserve)
	if memTotal <= reserve {
		return 0, fmt.Errorf("only %d bytes of memory; not limiting agents", memTotal)
	}
	memMax = memTotal - reserve
	// The controllers must be enabled for the parent's children; systemd
	// normally has, so a failure here is only worth reporting below.
	os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+memory +cpu"), 0o644)
	dir := filepath.Join(root, AgentsCgroup)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	for file, value := range map[string]string{
		"memory.max": strconv.FormatUint(memMax, 10),
		"cpu.weight": strconv.Itoa(agentsCPUWeight),
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o644); err != nil {
			return 0, fmt.Errorf("%s: %w", file, err)
		}
	}
	return memMax, nil
}

// MemTotal reads the host's memory size from /proc/meminfo.
func MemTotal() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb * 1024, err
		}
	}
	return 0, fmt.Errorf("no MemTotal in /proc/meminfo")
}
