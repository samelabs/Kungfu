package config

import (
	"net"
	"strings"
	"testing"
)

const s63Secret = "0123456789abcdef0123456789abcdef"

func s63Base(t *testing.T) {
	t.Helper()
	t.Setenv("DB_PASS", "pw")
	t.Setenv("SESSION_SECRET", s63Secret)
}

func TestS63InvalidTrustedProxyCIDRFailsClosed(t *testing.T) {
	s63Base(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,not-a-cidr")
	_, err := Load()
	if err == nil {
		t.Fatal("invalid TRUSTED_PROXY_CIDRS entry must fail Load")
	}
	if !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("error does not identify TRUSTED_PROXY_CIDRS: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "not-a-cidr") {
		t.Fatalf("error does not identify the offending entry: %s", err.Error())
	}
}

func TestS63TrustedProxyCIDRParsesAtLoad(t *testing.T) {
	s63Base(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,2001:db8::/32")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid CIDRs must load: %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 {
		t.Fatalf("parsed networks = %d, want 2", len(cfg.TrustedProxyCIDRs))
	}
	if !cfg.TrustedProxyCIDRs[0].Contains(net.ParseIP("10.1.2.3")) {
		t.Fatal("IPv4 CIDR not parsed as a network")
	}
	if !cfg.TrustedProxyCIDRs[1].Contains(net.ParseIP("2001:db8::1")) {
		t.Fatal("IPv6 CIDR not parsed as a network")
	}
}

func TestS63SingleTrustedProxyIPPreservesExistingContract(t *testing.T) {
	s63Base(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "192.0.2.10,2001:db8::7")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("bare literal IPs must load: %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 {
		t.Fatalf("networks = %d, want 2", len(cfg.TrustedProxyCIDRs))
	}
	if cfg.TrustedProxyCIDRs[0].String() != "192.0.2.10/32" {
		t.Fatalf("IPv4 literal = %s, want /32 host network", cfg.TrustedProxyCIDRs[0].String())
	}
	if cfg.TrustedProxyCIDRs[1].String() != "2001:db8::7/128" {
		t.Fatalf("IPv6 literal = %s, want /128 host network", cfg.TrustedProxyCIDRs[1].String())
	}
}

func TestS63DefaultTrustedProxiesRemainLoopback(t *testing.T) {
	s63Base(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	// empty env falls back to the documented default inside Load
	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("default must load: %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 {
		t.Fatalf("default networks = %d, want 2", len(cfg.TrustedProxyCIDRs))
	}
	if !cfg.TrustedProxyCIDRs[0].Contains(net.ParseIP("127.0.0.1")) {
		t.Fatal("default missing 127.0.0.0/8")
	}
	if !cfg.TrustedProxyCIDRs[1].Contains(net.ParseIP("::1")) {
		t.Fatal("default missing ::1/128")
	}
}
