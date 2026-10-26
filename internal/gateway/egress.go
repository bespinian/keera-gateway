package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Administrators choose where their models and MCP servers are. Without a
// guard, the gateway would reach anything its host can, such as the cloud
// metadata service, which hands out the host's credentials.
//
// The check runs on the dialled address, after DNS, so a name that resolves
// to a blocked address is blocked too. Behind a proxy, the proxy must filter.

// DefaultUpstreamDeny is loopback, link-local, the unspecified address and the
// cloud metadata addresses (AWS IPv6, Alibaba, Azure WireServer). Private
// ranges are allowed, since self-hosted models live there;
// KEERA_UPSTREAM_PRIVATE narrows them.
const DefaultUpstreamDeny = "127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128," +
	"fd00:ec2::254/128,100.100.100.200/32,168.63.129.16/32"

// ParsePrefixes reads a comma-separated list of addresses and prefixes, as in
// KEERA_UPSTREAM_DENY and KEERA_TRUSTED_PROXIES. "none" is an empty list.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
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

// ParsePrivateHosts reads KEERA_UPSTREAM_PRIVATE: the private host names
// organisations may reach, or "all" or "none".
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

// privateDial dials the named hosts with open, which applies deny, and every
// other host with policy.InsideNetwork denied too. That also covers loopback
// and link-local, in case an operator removed them from deny for a local
// backend.
//
// The name is checked before DNS, so a name an organisation controls cannot
// resolve into the network.
func privateDial(open *net.Dialer, deny []netip.Prefix, hosts []string,
) func(context.Context, string, string) (net.Conn, error) {
	why := "organisations may not reach addresses inside this network"
	if len(hosts) > 0 {
		why = "inside this network, organisations may only reach " + strings.Join(hosts, ", ")
	}
	closed := newDialer(denyDial(append(slices.Clone(deny), policy.InsideNetwork...),
		why+"; see KEERA_UPSTREAM_PRIVATE"))
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err == nil && slices.Contains(hosts, strings.ToLower(host)) {
			return open.DialContext(ctx, network, addr)
		}
		return closed.DialContext(ctx, network, addr)
	}
}

// denyDial is a net.Dialer Control that refuses an address in deny.
func denyDial(deny []netip.Prefix, why string) func(network, address string, _ syscall.RawConn) error {
	if len(deny) == 0 {
		return nil
	}
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		// A prefix never matches a zoned or IPv4-mapped address, so strip both.
		ip := ap.Addr().WithZone("").Unmap()
		for _, p := range deny {
			if p.Contains(ip) {
				return fmt.Errorf("the gateway does not connect to %s; %s", ip, why)
			}
		}
		return nil
	}
}
