package policy

import (
	"net/netip"
	"net/url"
	"strings"
)

// Hosting is which side of the customer's boundary a model is served on.
// It is derived from the backend URL rather than stored, so a repointed
// backend cannot make it wrong.
type Hosting string

const (
	// HostedInternal is a backend inside the deployment's own network: a
	// service name, a private address, the loopback.
	HostedInternal Hosting = "internal"
	// HostedExternal is a backend reached over the public internet. Prompts
	// sent to it leave.
	HostedExternal Hosting = "external"
	// HostedUnknown is a model with no backend, or a backend URL this cannot
	// read. A screen that says what leaves must not guess.
	HostedUnknown Hosting = "unknown"
)

// Hosting says whether this model's prompts stay inside the network. With
// several backends, external beats unknown and unknown beats internal: the
// answer is only as good as the weakest backend.
func (m Model) Hosting() Hosting {
	if len(m.Backends) == 0 {
		return HostedUnknown
	}
	out := HostedInternal
	for _, b := range m.Backends {
		switch h, _ := hostingOf(b); h {
		case HostedExternal:
			return HostedExternal
		case HostedUnknown:
			out = HostedUnknown
		}
	}
	return out
}

// Endpoint is the host that decided Hosting, so the screen can show the reason
// for its mark. The path and any credential in the URL are left out.
func (m Model) Endpoint() string {
	var first string
	for _, b := range m.Backends {
		h, host := hostingOf(b)
		if h == HostedExternal {
			return host
		}
		if first == "" {
			first = host
		}
	}
	return first
}

// internalSuffixes are domains that do not resolve outside a network:
// Kubernetes' cluster domain, mDNS, and the usual private ones.
var internalSuffixes = []string{
	".local", ".localdomain", ".localhost", ".internal", ".intranet",
	".lan", ".home.arpa", ".svc", ".cluster.local",
}

// hostingOf classifies one backend URL and returns the host it classified.
func hostingOf(raw string) (Hosting, string) {
	u, ok := parseBackend(raw)
	if !ok {
		return HostedUnknown, ""
	}
	name := u.Hostname()
	if name == "" {
		return HostedUnknown, u.Host
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		return addrHosting(addr), u.Host
	}
	return nameHosting(name), u.Host
}

// parseBackend parses a backend URL that has a host.
func parseBackend(raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	// A bare host like "vllm:8000" parses with no host at all, so read it
	// again as the host it is.
	if u.Host == "" {
		u, err = url.Parse("//" + raw)
		if err != nil {
			return nil, false
		}
	}
	return u, u.Host != ""
}

func addrHosting(addr netip.Addr) Hosting {
	switch {
	case addr.IsLoopback(), addr.IsPrivate(), addr.IsUnspecified(),
		addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(),
		// Carrier-grade NAT, which Tailscale addresses use. It is not
		// routable on the public internet either.
		addr.Is4() && addr.As4()[0] == 100 && addr.As4()[1] >= 64 && addr.As4()[1] <= 127:
		return HostedInternal
	}
	return HostedExternal
}

func nameHosting(name string) Hosting {
	lower := strings.ToLower(strings.TrimSuffix(name, "."))
	// A single-label host like "vllm" never resolves publicly.
	if !strings.Contains(lower, ".") {
		return HostedInternal
	}
	for _, s := range internalSuffixes {
		if strings.HasSuffix(lower, s) {
			return HostedInternal
		}
	}
	return HostedExternal
}
