// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// HostAllowlist refuses HTTP requests whose Host header does not name
// hmbridge. This stops DNS rebinding: a web site whose DNS name is
// re-pointed at hmbridge's address after the page loaded would
// otherwise be same-origin with the status page (read the JSON API,
// start pairing) and could scrape /metrics. A browser always sends the
// name it resolved, which is not on the list.
//
// Accepted are IP address literals (a browser that connects to an IP
// address sends that address: no rebinding is possible), "localhost",
// the machine's host name (also its first label and with ".local", for
// mDNS), the host name of the listen address, and the extra names
// configured in http_allowed_hosts ("*" accepts every name).
type HostAllowlist struct {
	names map[string]bool
	any   bool
}

// NewHostAllowlist returns the allowlist for a server listening on
// listen ("host:port") with the extra names (http_allowed_hosts).
func NewHostAllowlist(listen string, extra []string) *HostAllowlist {
	a := &HostAllowlist{names: map[string]bool{"localhost": true}}
	if h, err := os.Hostname(); err == nil {
		a.addHostname(h)
	}
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		a.add(h)
	}
	for _, h := range extra {
		if h == "*" {
			a.any = true
			continue
		}
		a.add(h)
	}
	return a
}

func (a *HostAllowlist) add(h string) {
	if h = normalizeHost(h); h != "" {
		a.names[h] = true
	}
}

// addHostname adds the machine's host name, its first label and both
// with ".local" (mDNS, e.g. raspberrypi.local).
func (a *HostAllowlist) addHostname(h string) {
	h = normalizeHost(h)
	if h == "" {
		return
	}
	short, _, _ := strings.Cut(h, ".")
	for _, n := range []string{h, short} {
		a.add(n)
		a.add(n + ".local")
	}
}

// normalizeHost lower-cases h and strips a port, IPv6 brackets and a
// trailing dot.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// Allowed reports whether a request with the Host header host is for
// hmbridge.
func (a *HostAllowlist) Allowed(host string) bool {
	if a.any {
		return true
	}
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return a.names[h]
}

// Handler wraps h: requests for other host names get 421 Misdirected
// Request.
func (a *HostAllowlist) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Allowed(r.Host) {
			http.Error(w, "hmbridge: unknown host name; use the IP address, or add the name to http_allowed_hosts in the configuration", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}
