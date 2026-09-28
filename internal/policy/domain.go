package policy

import (
	"fmt"
	"strings"
)

// CleanEmailDomain turns what was typed into what a sign-in is compared
// against: the lowercased part of an address after the @, as
// authn.Identity.Domain returns it.
//
// Anything else is refused now. A domain that matches nothing fails silently,
// and only once a second organisation exists.
func CleanEmailDomain(given string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(given))
	// "@example.ch" and the fully-qualified "example.ch." both mean the domain.
	d = strings.TrimSuffix(strings.TrimPrefix(d, "@"), ".")
	if d == "" {
		return "", nil
	}
	if at := strings.LastIndexByte(d, '@'); at >= 0 {
		return "", fmt.Errorf("an email domain is a domain, not an address; "+
			"for %s that is %s", given, d[at+1:])
	}
	if strings.ContainsAny(d, ":/") {
		return "", fmt.Errorf("an email domain is a domain, not a URL; "+
			"for %s that is %s", given, hostOf(d))
	}
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return "", fmt.Errorf("%q is not an email domain: %q cannot appear in one", given, r)
		}
	}
	if strings.HasPrefix(d, "-") || strings.HasPrefix(d, ".") ||
		strings.HasSuffix(d, "-") || strings.Contains(d, "..") {
		return "", fmt.Errorf("%q is not an email domain", given)
	}
	return d, nil
}

// hostOf is the host part of something typed as a URL.
func hostOf(s string) string {
	if _, after, ok := strings.Cut(s, "//"); ok {
		s = after
	}
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, ":")
	return s
}
