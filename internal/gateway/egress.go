package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
)

// The addresses the gateway will not connect to.
//
// An organisation's administrator sets where its models and MCP servers are,
// and the gateway sends requests there and passes the answers back. Without a
// guard that reaches whatever the gateway's own host can: the cloud's metadata
// service, which hands out the host's credentials, or a port only meant for
// the host itself.
//
// The check runs on the address being dialled, after DNS, so a name that
// resolves to a blocked address is blocked too. Through a proxy, the address
// dialled is the proxy's, and the proxy has to do its own filtering.

// DefaultUpstreamDeny is loopback, link-local, where the clouds serve their
// metadata, the unspecified address, AWS's IPv6 metadata address, Alibaba's
// metadata address and Azure's WireServer. Private ranges are allowed: that is
// where a self-hosted inference plane is. KEERA_UPSTREAM_PRIVATE narrows them.
const DefaultUpstreamDeny = "127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128," +
	"fd00:ec2::254/128,100.100.100.200/32,168.63.129.16/32"

// privateRanges are the addresses inside a network: private, carrier-grade
// NAT, unique local IPv6, and loopback and link-local, which an operator may
// have taken off the deny list for a backend on the same host. A limited
// organisation is kept from all of them, and with KEERA_UPSTREAM_PRIVATE every
// organisation is kept from all but the hosts it names.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// ParseUpstreamDeny reads a comma-separated list of addresses and prefixes.
// "none" denies nothing.
func ParseUpstreamDeny(s string) ([]netip.Prefix, error) {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return nil, nil
	}
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("%q is not an address or a prefix", part)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a prefix", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ParsePrivateHosts reads KEERA_UPSTREAM_PRIVATE: the host names inside the
// network that organisations may reach. "all" lets them reach every private
// address, and "none" none at all.
func ParsePrivateHosts(s string) (limit bool, hosts []string, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all":
		return false, nil, nil
	case "none":
		return true, nil, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, ":/") {
			return false, nil, fmt.Errorf("%q is not a host name; name the host alone, "+
				"such as keera-engine", part)
		}
		hosts = append(hosts, part)
	}
	return true, hosts, nil
}

// privateDial dials the hosts named in hosts with deny, and every other host
// with the private ranges denied too.
//
// The name is checked before DNS, and the address after it. A name an
// organisation controls cannot be one of these, so it cannot resolve into
// the network.
func privateDial(deny []netip.Prefix, hosts []string) func(context.Context, string, string) (net.Conn, error) {
	open := newDialer(denyDial(deny, "KEERA_UPSTREAM_DENY blocks it"))
	why := "organisations may not reach addresses inside this network"
	if len(hosts) > 0 {
		why = "inside this network, organisations may only reach " + strings.Join(hosts, ", ")
	}
	closed := newDialer(denyDial(append(slices.Clone(deny), privateRanges...),
		why+"; see KEERA_UPSTREAM_PRIVATE"))
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err == nil && slices.Contains(hosts, strings.ToLower(host)) {
			return open.DialContext(ctx, network, addr)
		}
		return closed.DialContext(ctx, network, addr)
	}
}

// denyDial is a net.Dialer Control that refuses an address in deny, saying
// why.
func denyDial(deny []netip.Prefix, why string) func(network, address string, _ syscall.RawConn) error {
	if len(deny) == 0 {
		return nil
	}
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		// A prefix never contains an address with a zone, or an IPv4 address
		// written as IPv6, so both are taken off first.
		ip := ap.Addr().WithZone("").Unmap()
		for _, p := range deny {
			if p.Contains(ip) {
				return fmt.Errorf("the gateway does not connect to %s; %s", ip, why)
			}
		}
		return nil
	}
}
