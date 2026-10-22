// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHostAllowlist(t *testing.T) {
	a := NewHostAllowlist("pi.lan:8013", []string{"HMBridge.Example.org", "proxy.local."})
	host, _ := os.Hostname()
	host = strings.ToLower(host)
	short, _, _ := strings.Cut(host, ".")
	for _, h := range []string{
		"localhost", "localhost:8013", "LOCALHOST.", "127.0.0.1", "127.0.0.1:8013", "192.168.1.20:8013",
		"[::1]:8013", "[fe80::1%25eth0]:8013", "::1", "pi.lan", "pi.lan:80", "hmbridge.example.org:8013",
		"proxy.local", host, short + ".local:8013",
	} {
		if !a.Allowed(h) {
			t.Errorf("%q refused", h)
		}
	}
	// DNS rebinding: the attacker's name, resolved to hmbridge's address.
	for _, h := range []string{"", "evil.example", "evil.example:8013", "127.0.0.1.evil.example", "localhost.evil.example", "pi.lan.evil.example", "[evil]"} {
		if a.Allowed(h) {
			t.Errorf("%q allowed", h)
		}
	}
	if !NewHostAllowlist(":8013", []string{"*"}).Allowed("evil.example") {
		t.Error(`"*" does not allow every name`)
	}

	h := a.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for host, want := range map[string]int{"evil.example:8013": http.StatusMisdirectedRequest, "127.0.0.1:8013": http.StatusOK} {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", host, rec.Code, want)
		}
	}
}
