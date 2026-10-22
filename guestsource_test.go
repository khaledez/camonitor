package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSourceUsesForwardedHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", " 1.2.3.4, 10.0.0.1")
	if ip, _ := clientSource(r); ip != "1.2.3.4" {
		t.Fatalf("ip = %q", ip)
	}
}

func TestClientSourceFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "9.9.9.9:5555"
	if ip, _ := clientSource(r); ip != "9.9.9.9" {
		t.Fatalf("ip = %q", ip)
	}
	r.RemoteAddr = "8.8.8.8"
	if ip, _ := clientSource(r); ip != "8.8.8.8" {
		t.Fatalf("no-port ip = %q", ip)
	}
}

func TestClientSourceDeviceFromUserAgent(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("User-Agent", "  Mozilla/5.0  ")
	if _, d := clientSource(r); d != "Mozilla/5.0" {
		t.Fatalf("device = %q", d)
	}
	r.Header.Set("User-Agent", strings.Repeat("a", 200))
	if _, d := clientSource(r); len(d) != 120 {
		t.Fatalf("device len = %d", len(d))
	}
}

func TestClientSourceEmptyWhenNoUA(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if _, d := clientSource(r); d != "" {
		t.Fatalf("device = %q", d)
	}
}
