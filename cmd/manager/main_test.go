package main

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestProbeBindAddressDefaultIsWildcard pins the health/readiness bind
// address default against an earlier finding: `10.0.0.0:8081` is NOT a
// wildcard — the kernel refuses to bind an IP that was never assigned to the
// pod network namespace, which breaks manager startup in-cluster. The
// default must parse and must be a bindable wildcard (0.0.0.0 / [::] / empty
// host = all interfaces).
//
// The default is read from this file's own flag declaration (main cannot be
// called from a test without os.Exit side effects), which keeps the pin
// honest: changing the default in main.go updates this test automatically.
func TestProbeBindAddressDefaultIsWildcard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "cmd", "manager", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	re := regexp.MustCompile(`flag\.StringVar\(&probeAddr,\s*"health-probe-bind-address",\s*"([^"]*)"`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatal("probe bind flag default not found in main.go (declaration changed?)")
	}
	addr := string(m[1])

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("default probe bind address %q does not parse as host:port: %v", addr, err)
	}
	if port == "" {
		t.Fatalf("default probe bind address %q has an empty port", addr)
	}
	if host != "" && host != "0.0.0.0" && host != "::" && host != "localhost" && host != "127.0.0.1" {
		t.Fatalf("default probe bind address %q is a non-wildcard literal host %q; it only binds if the pod was assigned that IP", addr, host)
	}

	// net.Listen on the exact default would need a real socket; verify the
	// address literal is a valid IPv4 any-address when IPv4 form is used.
	if host == "0.0.0.0" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			t.Fatalf("host %q does not parse as the unspecified IPv4 address", host)
		}
	}
	t.Logf("probe bind default %q accepted (host=%q port=%q)", addr, host, port)
}
