package main

import (
	"net"
	"net/http"
	"strings"
)

const maxDeviceLen = 120

// clientSource is the single place that decides where a guest request came from.
// Behind Tailscale Funnel the real client IP may only be in a forwarding header;
// adjust here once verified against a live Funnel request.
func clientSource(r *http.Request) (ip string, device string) {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		firstHop, _, _ := strings.Cut(forwarded, ",")
		ip = strings.TrimSpace(firstHop)
	}
	if ip == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip = host
	}

	device = strings.TrimSpace(r.UserAgent())
	if len(device) > maxDeviceLen {
		device = device[:maxDeviceLen]
	}
	return ip, device
}
