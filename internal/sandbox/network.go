package sandbox

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Network wires agent sandboxes to a host bridge.
//
// Each agent gets its own network namespace with one veth into the bridge.
// Bridge ports are isolated from each other, nftables drops anything from
// the bridge addressed to the host or to private ranges, and outbound
// traffic is masqueraded. Agents reach the internet and nothing else.
type Network struct {
	Bridge string
	Subnet netip.Prefix
}

// Gateway is the host's address on the bridge.
func (n *Network) Gateway() netip.Addr { return n.Subnet.Masked().Addr().Next() }

// NamespacePath is where the agent's network namespace is pinned.
func (n *Network) NamespacePath(id string) string { return "/var/run/netns/" + nsName(id) }

func nsName(id string) string   { return "hm-" + id }
func vethName(id string) string { return "hv" + id }

// Setup creates the bridge and firewall rules. It is idempotent and runs
// at every start, so rules edited by hand are put back.
func (n *Network) Setup(ctx context.Context) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	if _, err := command(ctx, "ip", "link", "show", n.Bridge); err != nil {
		if _, err := command(ctx, "ip", "link", "add", n.Bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	gw := netip.PrefixFrom(n.Gateway(), n.Subnet.Bits()).String()
	if _, err := command(ctx, "ip", "addr", "replace", gw, "dev", n.Bridge); err != nil {
		return err
	}
	if _, err := command(ctx, "ip", "link", "set", n.Bridge, "up"); err != nil {
		return err
	}
	return commandInput(ctx, n.ruleset(), "nft", "-f", "-")
}

func (n *Network) ruleset() string {
	r := strings.NewReplacer("BRIDGE", n.Bridge, "SUBNET", n.Subnet.Masked().String())
	return r.Replace(`
table inet hermitator
delete table inet hermitator
table inet hermitator {
	chain input {
		type filter hook input priority 0; policy accept;
		iifname "BRIDGE" ct state established,related accept
		iifname "BRIDGE" drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "BRIDGE" ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 } drop
		oifname "BRIDGE" ct state established,related accept
		oifname "BRIDGE" drop
	}
	chain postrouting {
		type nat hook postrouting priority 100;
		ip saddr SUBNET oifname != "BRIDGE" masquerade
	}
}
`)
}

// Attach creates (or repairs) an agent's namespace and veth and assigns
// its address. runsc strips addresses and routes from the namespace when
// it takes the interface over, so this runs before every boot and restore.
func (n *Network) Attach(ctx context.Context, id string, ip netip.Addr) error {
	ns, veth := nsName(id), vethName(id)
	if _, err := os.Stat(n.NamespacePath(id)); err != nil {
		if _, err := command(ctx, "ip", "netns", "add", ns); err != nil {
			return err
		}
	}
	if _, err := command(ctx, "ip", "link", "show", veth); err != nil {
		if _, err := command(ctx, "ip", "link", "add", veth, "type", "veth", "peer", "name", "eth0", "netns", ns); err != nil {
			return err
		}
	}
	steps := [][]string{
		{"ip", "link", "set", veth, "master", n.Bridge, "up"},
		{"bridge", "link", "set", "dev", veth, "isolated", "on"},
		{"ip", "-n", ns, "addr", "replace", netip.PrefixFrom(ip, n.Subnet.Bits()).String(), "dev", "eth0"},
		{"ip", "-n", ns, "link", "set", "eth0", "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "replace", "default", "via", n.Gateway().String()},
	}
	for _, s := range steps {
		if _, err := command(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// Detach removes an agent's namespace and veth.
func (n *Network) Detach(ctx context.Context, id string) error {
	command(ctx, "ip", "link", "del", vethName(id))
	if _, err := os.Stat(n.NamespacePath(id)); err == nil {
		if _, err := command(ctx, "ip", "netns", "del", nsName(id)); err != nil {
			return err
		}
	}
	return nil
}
