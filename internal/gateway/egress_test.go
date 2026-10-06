package gateway

import (
	"testing"
)

func TestTheDefaultDenyListBlocksTheHostAndTheMetadataService(t *testing.T) {
	deny, err := ParseUpstreamDeny(DefaultUpstreamDeny)
	if err != nil {
		t.Fatal(err)
	}
	check := denyDial(deny)
	for _, addr := range []string{
		"127.0.0.1:8080", "[::1]:8080", "169.254.169.254:80", "[fd00:ec2::254]:80",
		"0.0.0.0:80", "[fe80::1%eth0]:80",
		// IPv4 written as IPv6 is still loopback.
		"[::ffff:127.0.0.1]:80",
	} {
		if check("tcp", addr, nil) == nil {
			t.Errorf("%s was not blocked", addr)
		}
	}
	for _, addr := range []string{"10.0.0.7:8000", "192.168.1.2:443", "[2001:db8::1]:443", "8.8.8.8:443"} {
		if err := check("tcp", addr, nil); err != nil {
			t.Errorf("%s was blocked: %v", addr, err)
		}
	}
}

func TestADenyListTakesAddressesAndPrefixes(t *testing.T) {
	deny, err := ParseUpstreamDeny(" 10.1.2.3 , 192.168.0.0/16,,2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	check := denyDial(deny)
	for _, addr := range []string{"10.1.2.3:80", "192.168.4.5:80", "[2001:db8::7]:80"} {
		if check("tcp", addr, nil) == nil {
			t.Errorf("%s was not blocked", addr)
		}
	}
	if err := check("tcp", "10.1.2.4:80", nil); err != nil {
		t.Errorf("a neighbouring address was blocked: %v", err)
	}
}

func TestNoneDeniesNothing(t *testing.T) {
	deny, err := ParseUpstreamDeny("none")
	if err != nil || deny != nil || denyDial(deny) != nil {
		t.Errorf("none = %v, %v; want no list and no check", deny, err)
	}
}

func TestATypoInTheDenyListIsAnError(t *testing.T) {
	for _, s := range []string{"127.0.0.1/33", "localhost", "10.0.0.0/8,nope"} {
		if _, err := ParseUpstreamDeny(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}
