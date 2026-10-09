package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTheDefaultDenyListBlocksTheHostAndTheMetadataService(t *testing.T) {
	deny, err := ParseUpstreamDeny(DefaultUpstreamDeny)
	if err != nil {
		t.Fatal(err)
	}
	check := denyDial(deny, "KEERA_UPSTREAM_DENY blocks it")
	for _, addr := range []string{
		"127.0.0.1:8080", "[::1]:8080", "169.254.169.254:80", "[fd00:ec2::254]:80",
		"0.0.0.0:80", "[fe80::1%eth0]:80", "100.100.100.200:80", "168.63.129.16:80",
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
	check := denyDial(deny, "KEERA_UPSTREAM_DENY blocks it")
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
	if err != nil || deny != nil || denyDial(deny, "KEERA_UPSTREAM_DENY blocks it") != nil {
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

func TestOnlyTheNamedHostsReachThePrivateNetwork(t *testing.T) {
	limit, hosts, err := ParsePrivateHosts(" Keera-Engine , 10.255.255.1")
	if err != nil || !limit || len(hosts) != 2 || hosts[0] != "keera-engine" {
		t.Fatalf("ParsePrivateHosts = %v, %v, %v", limit, hosts, err)
	}
	dial := privateDial(nil, hosts)
	blocked := func(addr string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		conn, err := dial(ctx, "tcp", addr)
		if conn != nil {
			conn.Close()
		}
		return err != nil && strings.Contains(err.Error(), "KEERA_UPSTREAM_PRIVATE")
	}
	for _, addr := range []string{"10.0.0.7:8000", "192.168.1.2:443", "100.64.0.1:80", "[fd12::1]:80"} {
		if !blocked(addr) {
			t.Errorf("%s was not blocked", addr)
		}
	}
	if blocked("10.255.255.1:9") {
		t.Error("a named host was blocked")
	}
}

func TestPrivateHostsAreNamesOrAllOrNone(t *testing.T) {
	if limit, _, err := ParsePrivateHosts("all"); err != nil || limit {
		t.Errorf("all = %v, %v; want no limit", limit, err)
	}
	if limit, hosts, err := ParsePrivateHosts("none"); err != nil || !limit || hosts != nil {
		t.Errorf("none = %v, %v, %v; want a limit and no hosts", limit, hosts, err)
	}
	for _, s := range []string{"http://keera-engine", "keera-engine:8000", "10.0.0.0/8"} {
		if _, _, err := ParsePrivateHosts(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}
