package gateway

import (
	"fmt"
	"net/netip"
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

// DefaultUpstreamDeny is loopback, link-local, where every major cloud serves
// its metadata, the unspecified address, and AWS's IPv6 metadata address.
// Private ranges are allowed: that is where a self-hosted inference plane is.
const DefaultUpstreamDeny = "127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128,fd00:ec2::254/128"

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

// denyDial is a net.Dialer Control that refuses an address in deny.
func denyDial(deny []netip.Prefix) func(network, address string, _ syscall.RawConn) error {
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
				return fmt.Errorf("the gateway does not connect to %s; KEERA_UPSTREAM_DENY blocks it", ip)
			}
		}
		return nil
	}
}
