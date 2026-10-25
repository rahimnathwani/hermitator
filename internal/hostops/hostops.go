// Package hostops answers /host: what the server itself is doing, read
// from the machine rather than from any agent. Nothing here takes input
// that reaches a shell; the commands it runs are fixed.
package hostops

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/media"
)

// Host reports on the machine hermitator runs on.
type Host struct {
	// Build is the running version, as `hermitator version` prints it.
	Build  string
	Config *config.Config
	// Started is when this process started, for its uptime.
	Started time.Time
	// Unit is the systemd unit whose journal /host logs reads.
	Unit string
	// Exit ends the process so systemd starts it again.
	Exit func()
	// Connected reports whether a WhatsApp account is connected.
	Connected func(accountID string) bool
	// Accounts lists every linked account's ID.
	Accounts func(ctx context.Context) []string
}

// Version is the running build's.
func (h *Host) Version() string { return h.Build }

// Facts are the lines of /host: version, uptime, load, memory, disk.
// Lines the machine cannot answer (no /proc on macOS) are left out.
func (h *Host) Facts(ctx context.Context) []string {
	var out []string
	line := fmt.Sprintf("hermitator %s, up %s, runtime %s", h.Build, roughly(time.Since(h.Started)), h.Config.Runtime)
	if h.Config.Runtime == "runsc" {
		if v := runscVersion(ctx, h.Config.Runsc); v != "" {
			line += " (" + v + ")"
		}
	}
	out = append(out, line)
	if up := machineUptime(); up > 0 {
		out = append(out, "Machine up "+roughly(up))
	}
	if load, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(load))
		if len(f) >= 3 {
			out = append(out, fmt.Sprintf("Load: %s %s %s on %d CPUs", f[0], f[1], f[2], runtime.NumCPU()))
		}
	}
	if mem := meminfo(); mem["MemTotal"] > 0 {
		s := fmt.Sprintf("Memory: %s free of %s", media.HumanSize(mem["MemAvailable"]), media.HumanSize(mem["MemTotal"]))
		if mem["SwapTotal"] > 0 {
			s += fmt.Sprintf("; swap %s used of %s", media.HumanSize(mem["SwapTotal"]-mem["SwapFree"]), media.HumanSize(mem["SwapTotal"]))
		}
		out = append(out, s)
	}
	if free, total, err := diskSpace(h.Config.DataDir); err == nil {
		out = append(out, fmt.Sprintf("Disk: %s free of %s under %s", media.HumanSize(free), media.HumanSize(total), h.Config.DataDir))
	}
	if h.Accounts != nil && h.Connected != nil {
		ids := h.Accounts(ctx)
		up := 0
		for _, id := range ids {
			if h.Connected(id) {
				up++
			}
		}
		out = append(out, fmt.Sprintf("WhatsApp: %d of %d numbers connected", up, len(ids)))
	}
	return out
}

// Doctor runs `hermitator doctor`'s checks, formatted for a chat.
func (h *Host) Doctor(ctx context.Context) (string, bool) {
	checks := Doctor(ctx, h.Config)
	var b strings.Builder
	for _, c := range checks {
		switch {
		case c.Note != "":
			fmt.Fprintf(&b, "➖ %s: %s\n", c.Name, c.Note)
		case c.Err == nil:
			fmt.Fprintf(&b, "✅ %s\n", c.Name)
		default:
			fmt.Fprintf(&b, "❌ %s: %v\n", c.Name, c.Err)
			if c.Hint != "" {
				fmt.Fprintf(&b, "    %s\n", c.Hint)
			}
		}
	}
	return strings.TrimSpace(b.String()), !Failed(checks)
}

// Logs returns the last n warnings and errors from the unit's journal, one
// short line each. The caller redacts them before they leave the host.
func (h *Host) Logs(ctx context.Context, n int) (string, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return "", fmt.Errorf("this host has no journalctl")
	}
	unit := h.Unit
	if unit == "" {
		unit = "hermitator"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// slog writes JSON to stdout, which journald files as info whatever
	// the level, so read a stretch of the journal and filter on the
	// level field instead of journald's priority.
	out, err := exec.CommandContext(ctx, "journalctl", "--unit", unit, "--lines", strconv.Itoa(min(n*50, 5000)),
		"--output", "cat", "--no-pager", "--quiet").Output()
	if err != nil {
		return "", fmt.Errorf("journalctl: %w", err)
	}
	return FormatLogs(string(out), n), nil
}

// FormatLogs turns slog's JSON lines into short ones, keeping the last n
// at WARN or above. Lines that are not JSON (a panic, systemd starting
// and stopping the unit) are kept as they are.
func FormatLogs(raw string, n int) string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(text), &rec) != nil {
			lines = append(lines, clip(text, 300))
			continue
		}
		level, _ := rec["level"].(string)
		if level != "WARN" && level != "ERROR" {
			continue
		}
		lines = append(lines, logLine(rec))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return "No warnings or errors in the recent log."
	}
	return strings.Join(lines, "\n")
}

func logLine(rec map[string]any) string {
	var b strings.Builder
	if t, err := time.Parse(time.RFC3339Nano, fmt.Sprint(rec["time"])); err == nil {
		b.WriteString(t.UTC().Format("01-02 15:04:05 "))
	}
	fmt.Fprintf(&b, "%v %v", rec["level"], rec["msg"])
	keys := make([]string, 0, len(rec))
	for k := range rec {
		if k != "time" && k != "level" && k != "msg" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, rec[k])
	}
	return clip(strings.ReplaceAll(b.String(), "\n", " "), 300)
}

// Restart ends the process; systemd's Restart=on-failure starts it again.
// Sandboxes are separate processes (KillMode=process) and keep running.
func (h *Host) Restart() {
	if h.Exit != nil {
		h.Exit()
	}
}

func runscVersion(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "runsc version"))
}

func machineUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	s, _ := strconv.ParseFloat(f[0], 64)
	return time.Duration(s * float64(time.Second))
}

// meminfo reads /proc/meminfo in bytes.
func meminfo() map[string]int64 {
	out := map[string]int64{}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(f[0], 10, 64)
		out[k] = n << 10 // kB
	}
	return out
}

func diskSpace(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bs), int64(uint64(st.Blocks) * bs), nil
}

// roughly is a duration in its two largest units: "3d 4h", "12m".
func roughly(d time.Duration) string {
	d = d.Round(time.Minute)
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
