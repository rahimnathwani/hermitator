package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/rahimnathwani/hermitator/internal/turn"
)

// Capabilities inside the sandbox. gVisor implements them in its own
// kernel, so they grant nothing on the host; this is Docker's default set
// plus ptrace for debuggers.
var capabilities = []string{
	"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID",
	"CAP_KILL", "CAP_MKNOD", "CAP_NET_BIND_SERVICE", "CAP_SETFCAP", "CAP_SETGID",
	"CAP_SETPCAP", "CAP_SETUID", "CAP_SYS_CHROOT", "CAP_SYS_PTRACE",
}

// Trust files in the tools bundle, and where sandboxes see them.
const (
	EgressCAFile = "egress-ca.crt"
	CABundleFile = "ca-bundle.crt"
	guestLib     = turn.ToolsDir + "/lib/"
)

// trustEnv points every TLS stack in the sandbox at the egress CA, when
// the tools bundle it mounts has one: Node's extra CA, and a full bundle
// (the system roots plus the CA) for OpenSSL, Go, curl, git and Python.
func trustEnv(tools string) []string {
	if tools == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(tools, "lib", EgressCAFile)); err != nil {
		return nil
	}
	env := []string{"NODE_EXTRA_CA_CERTS=" + guestLib + EgressCAFile}
	if _, err := os.Stat(filepath.Join(tools, "lib", CABundleFile)); err == nil {
		for _, k := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE"} {
			env = append(env, k+"="+guestLib+CABundleFile)
		}
	}
	return env
}

// TrustsEgress reports whether the sandbox's current configuration, the
// one a restore reuses, trusts the egress CA. Only then may its turns
// hold placeholders instead of credentials.
func (s *Spec) TrustsEgress() bool {
	b, err := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if err != nil {
		return false
	}
	var spec specs.Spec
	if json.Unmarshal(b, &spec) != nil || spec.Process == nil {
		return false
	}
	for _, kv := range spec.Process.Env {
		if strings.HasPrefix(kv, "NODE_EXTRA_CA_CERTS=") {
			return true
		}
	}
	return false
}

// guestEnv is the environment of PID 1 and so of every job: hermitator's
// fixed variables, trust for the egress CA if tools has it, then whatever
// the image declared.
func guestEnv(img Image, tools string) []string {
	env := []string{
		"PATH=/opt/hermitator/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"USER=root",
		"LANG=C.UTF-8",
		"TERM=xterm-256color",
		"DISABLE_AUTOUPDATER=1",
		"HERMITATOR_SPOOL=" + turn.SpoolDir,
		"HERMITATOR_SHARED=" + turn.SharedDir,
		"HERMITATOR_WORKSPACE=" + turn.Workspace,
	}
	env = append(env, trustEnv(tools)...)
	seen := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[k] = true
	}
	for _, kv := range img.Env {
		if k, _, ok := strings.Cut(kv, "="); ok && !seen[k] {
			env = append(env, kv)
			seen[k] = true
		}
	}
	return env
}

// ociSpec builds the bundle config for a sandbox.
func ociSpec(s *Spec, netnsPath string) *specs.Spec {
	memory := int64(s.MemoryMB) << 20
	pids := int64(8192)
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			User: specs.User{UID: 0, GID: 0},
			Args: []string{turn.ToolsDir + "/bin/hermitator-guest", "init"},
			Env:  guestEnv(s.Image, s.Tools),
			Cwd:  "/",
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  capabilities,
				Effective: capabilities,
				Permitted: capabilities,
			},
			Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1 << 20, Soft: 1 << 20}},
		},
		Root:     &specs.Root{Path: s.merged()},
		Hostname: "agent",
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			// Chromium needs far more shared memory than the 64 MB default.
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "nodev", "mode=1777", "size=1g"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			// runsc's default for bind mounts other than the root is
			// --file-access-mounts=shared, so files the host writes here
			// are visible to a running sandbox and vice versa.
			{Destination: turn.SharedDir, Type: "bind", Source: s.SharedDir(), Options: []string{"rbind", "rw"}},
			{Destination: turn.SpoolDir, Type: "bind", Source: s.SpoolDir(), Options: []string{"rbind", "rw"}},
			{Destination: turn.ToolsDir, Type: "bind", Source: s.Tools, Options: []string{"rbind", "ro"}},
		},
		Linux: &specs.Linux{
			CgroupsPath: "/hermitator/" + s.ID,
			Resources: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{Limit: &memory},
				Pids:   &specs.LinuxPids{Limit: &pids},
			},
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace, Path: netnsPath},
			},
		},
	}
}

func writeSpec(s *Spec, spec *specs.Spec) error {
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, ".config.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, "config.json"))
}

// BootedTools is the guest bundle the sandbox's current boot mounts, which
// a checkpointed sandbox keeps until it boots afresh; empty when the
// runtime keeps no bundle config.
func (s *Spec) BootedTools() string { return bundleTools(s.Dir) }

// bundleTools returns the guest bundle a sandbox's current config mounts,
// so the bundle is not deleted while a checkpoint still refers to it.
func bundleTools(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var spec specs.Spec
	if json.Unmarshal(b, &spec) != nil {
		return ""
	}
	for _, m := range spec.Mounts {
		if m.Destination == turn.ToolsDir {
			return m.Source
		}
	}
	return ""
}
