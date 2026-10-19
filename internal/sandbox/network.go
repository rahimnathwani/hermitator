package sandbox

import (
	"encoding/json"
	"context"
	"fmt"
	"net/netip"
	"os"
	"strconv"
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
	// EgressPort is where the egress proxy listens on the gateway. HTTPS
	// from agents in the egress set is redirected there; 0 turns it off.
	EgressPort int
}

// privateRanges are addresses agents may not reach: the host, private
// networks, link-local (cloud metadata) and multicast.
const privateRanges = "{ 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 }"

// Peer is an agent's place on the bridge: its veth and its address.
type Peer struct {
	ID string
	IP netip.Addr
	// Egress routes the agent's HTTPS through the egress proxy.
	Egress bool
}

// Gateway is the host's address on the bridge.
func (n *Network) Gateway() netip.Addr { return n.Subnet.Masked().Addr().Next() }

// NamespacePath is where the agent's network namespace is pinned.
func (n *Network) NamespacePath(id string) string { return "/var/run/netns/" + nsName(id) }

func nsName(id string) string   { return "hm-" + id }
func vethName(id string) string { return "hv" + id }

// Setup creates the bridge and firewall rules. It is idempotent and runs
// at every start, so rules edited by hand are put back. peers are every
// agent there is, running or not: the anti-spoofing rule drops traffic
// from a port whose address it does not know.
func (n *Network) Setup(ctx context.Context, peers ...Peer) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	if _, err := command(ctx, "ip", "link", "show", n.Bridge); err != nil {
		if _, err := command(ctx, "ip", "link", "add", n.Bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	// Drop any other address, such as one left from a previous subnet.
	gw := netip.PrefixFrom(n.Gateway(), n.Subnet.Bits()).String()
	if out, err := command(ctx, "ip", "-4", "-o", "addr", "show", "dev", n.Bridge); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			for i := 0; i+1 < len(f); i++ {
				if f[i] == "inet" && f[i+1] != gw {
					command(ctx, "ip", "addr", "del", f[i+1], "dev", n.Bridge)
				}
			}
		}
	}
	if _, err := command(ctx, "ip", "addr", "replace", gw, "dev", n.Bridge); err != nil {
		return err
	}
	if _, err := command(ctx, "ip", "link", "set", n.Bridge, "up"); err != nil {
		return err
	}
	// The sets are filled in the same transaction that creates them, so
	// a running agent's traffic is never dropped while they are empty.
	return commandInput(ctx, n.ruleset(peers), "nft", "-f", "-")
}

// allow lets an agent's port send from its own address, and only that.
func (n *Network) allow(ctx context.Context, id string, ip netip.Addr) error {
	_, err := command(ctx, "nft", "add", "element", "bridge", "hermitator", "ports", "{", vethName(id), ".", ip.String(), "}")
	return err
}

// SetEgress routes an agent's HTTPS through the egress proxy, or stops.
func (n *Network) SetEgress(ctx context.Context, ip netip.Addr, on bool) error {
	if n.EgressPort == 0 {
		return nil
	}
	verb := "add"
	if !on {
		verb = "delete"
	}
	_, err := command(ctx, "nft", verb, "element", "inet", "hermitator", "egress", "{", ip.String(), "}")
	if !on && err != nil {
		return nil // was not in the set
	}
	return err
}

func (n *Network) ruleset(peers []Peer) string {
	var ports, egressIPs []string
	for _, p := range peers {
		ports = append(ports, `"`+vethName(p.ID)+`" . `+p.IP.String())
		if p.Egress && n.EgressPort != 0 {
			egressIPs = append(egressIPs, p.IP.String())
		}
	}
	elements := func(list []string) string {
		if len(list) == 0 {
			return ""
		}
		return " elements = { " + strings.Join(list, ", ") + " };"
	}
	r := strings.NewReplacer("EGRESS_ELEMENTS", elements(egressIPs), "PORT_ELEMENTS", elements(ports),
		"BRIDGE", n.Bridge, "SUBNET", n.Subnet.Masked().String(), "GATEWAY", n.Gateway().String(),
		"EGRESS", strconv.Itoa(n.EgressPort), "PRIVATE", privateRanges)
	egress := ""
	if n.EgressPort != 0 {
		egress = `
	chain prerouting {
		type nat hook prerouting priority dstnat;
		iifname "BRIDGE" ip saddr @egress ip daddr != PRIVATE tcp dport 443 dnat ip to GATEWAY:EGRESS
	}`
	}
	return r.Replace(`
table inet hermitator
delete table inet hermitator
table inet hermitator {
	set egress { type ipv4_addr;EGRESS_ELEMENTS }
	chain input {
		type filter hook input priority 0; policy accept;
		iifname "BRIDGE" ct state established,related accept
		iifname "BRIDGE" ip saddr @egress ip daddr GATEWAY tcp dport EGRESS accept
		iifname "BRIDGE" drop
	}
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "BRIDGE" ip daddr PRIVATE drop
		oifname "BRIDGE" ct state established,related accept
		oifname "BRIDGE" drop
	}` + egress + `
	chain postrouting {
		type nat hook postrouting priority 100;
		ip saddr SUBNET oifname != "BRIDGE" masquerade
	}
}

# An agent can only send from its own address: the egress proxy, and
# anything else on the host, tells agents apart by it.
table bridge hermitator
delete table bridge hermitator
table bridge hermitator {
	set ports { type ifname . ipv4_addr;PORT_ELEMENTS }
	chain prerouting {
		type filter hook prerouting priority -300; policy accept;
		iifname "hv*" iifname . ip saddr != @ports drop
		iifname "hv*" ether type ip6 drop
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
		// runsc takes over whatever addresses it finds, so leave only one.
		{"ip", "-n", ns, "addr", "flush", "dev", "eth0"},
		{"ip", "-n", ns, "addr", "add", netip.PrefixFrom(ip, n.Subnet.Bits()).String(), "dev", "eth0"},
		{"ip", "-n", ns, "link", "set", "eth0", "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "replace", "default", "via", n.Gateway().String()},
	}
	for _, s := range steps {
		if _, err := command(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	return n.allow(ctx, id, ip)
}

// Detach removes an agent's namespace and veth, and its place in the
// anti-spoofing set.
func (n *Network) Detach(ctx context.Context, id string) error {
	if out, err := command(ctx, "nft", "-j", "list", "set", "bridge", "hermitator", "ports"); err == nil {
		for _, ip := range portAddrs(out, vethName(id)) {
			command(ctx, "nft", "delete", "element", "bridge", "hermitator", "ports", "{", vethName(id), ".", ip, "}")
		}
	}
	command(ctx, "ip", "link", "del", vethName(id))
	if _, err := os.Stat(n.NamespacePath(id)); err == nil {
		if _, err := command(ctx, "ip", "netns", "del", nsName(id)); err != nil {
			return err
		}
	}
	return nil
}

// portAddrs finds the addresses the anti-spoofing set allows for a veth,
// in `nft -j list set` output.
func portAddrs(listing []byte, veth string) []string {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []struct {
					Concat []string `json:"concat"`
				} `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if json.Unmarshal(listing, &doc) != nil {
		return nil
	}
	var out []string
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		for _, e := range item.Set.Elem {
			if len(e.Concat) == 2 && e.Concat[0] == veth {
				out = append(out, e.Concat[1])
			}
		}
	}
	return out
}
