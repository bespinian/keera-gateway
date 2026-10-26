package policy

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Hosting is which side of the customer's network a model is served on. It is
// derived from the backend URL, not stored, so a repointed backend cannot make
// it wrong.
type Hosting string

const (
	HostedInternal Hosting = "internal"
	HostedExternal Hosting = "external"
	// HostedUnknown is a model with no backend, or an unreadable backend URL.
	// A screen that says what leaves must not guess.
	HostedUnknown Hosting = "unknown"
)

// Hosting is only as good as the weakest backend: external beats unknown, and
// unknown beats internal.
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

func HostingOf(raw string) Hosting {
	h, _ := hostingOf(raw)
	return h
}

// LockedMessage is what an organisation that has not paid is told about
// something it may not use yet. It names the organisation, so a person in
// several knows which one to add credit to.
func LockedMessage(orgName string) string {
	if orgName == "" {
		orgName = "this organisation"
	}
	return orgName + " needs to add credit before it can use all features"
}

// LocationFromBackends is the location of a model that states none. A backend
// inside the network is onprem. An outside URL does not say which country, so
// it is an error.
func (m Model) LocationFromBackends() (string, error) {
	if m.Hosting() == HostedInternal {
		return LocationOnPrem, nil
	}
	return "", fmt.Errorf("location is required: %s is outside this network, and its "+
		"address does not say where it runs - state it, such as ch or usa", m.Endpoint())
}

// CheckBackend says what is wrong with a model's backends, or nil. The control
// API and template files share it.
func (m Model) CheckBackend() error {
	switch {
	case len(m.Backends) == 0:
		return errors.New("at least one backend is required")
	case m.BackendModel == "":
		return errors.New("'backend_model' is required - it is the name the inference " +
			"plane serves, which for vLLM is --served-model-name")
	}
	for _, b := range m.Backends {
		u, err := url.Parse(strings.TrimSpace(b))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("backend " + strconv.Quote(b) + " is not an http or https address, " +
				"such as http://vllm:8000/v1")
		}
	}
	return nil
}

// Endpoint is the host that decided Hosting, without path or credential.
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

// internalSuffixes are domains that do not resolve outside a network.
// ".local" covers Kubernetes' ".cluster.local".
var internalSuffixes = []string{
	".local", ".localdomain", ".localhost", ".internal", ".intranet",
	".lan", ".home.arpa", ".svc",
}

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

func parseBackend(raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	// A bare host like "vllm:8000" parses with no host, so read it again.
	if u.Host == "" {
		u, err = url.Parse("//" + raw)
		if err != nil {
			return nil, false
		}
	}
	return u, u.Host != ""
}

// InsideNetwork are the addresses not on the public internet, including
// carrier-grade NAT, which Tailscale uses. The gateway's dialers keep an
// organisation from them when it may not reach inside; see
// internal/gateway/egress.go.
var InsideNetwork = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
}

// addrHosting drops the zone and IPv4-in-IPv6 form first, since no prefix
// contains them.
func addrHosting(addr netip.Addr) Hosting {
	addr = addr.WithZone("").Unmap()
	for _, p := range InsideNetwork {
		if p.Contains(addr) {
			return HostedInternal
		}
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
