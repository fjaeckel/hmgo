// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/uartgw/fakemod"
)

func TestLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := limitListener(inner, 2)
	defer l.Close()
	accepted := make(chan net.Conn, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients = append(clients, c)
	}
	var got []net.Conn
	for i := 0; i < 2; i++ {
		select {
		case c := <-accepted:
			got = append(got, c)
		case <-time.After(5 * time.Second):
			t.Fatal("connection not accepted")
		}
	}
	select {
	case <-accepted:
		t.Fatal("third connection accepted while two are open")
	case <-time.After(100 * time.Millisecond):
	}
	got[0].Close()
	got[0].Close() // a second Close must not release twice
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("third connection not accepted after a close")
	}
	got[1].Close()
	l.Close()
	for range accepted {
	}
}

func TestHTTPServerLimits(t *testing.T) {
	s := newHTTPServer(http.NotFoundHandler())
	if s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.ReadHeaderTimeout <= 0 || s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 64<<10 {
		t.Errorf("server limits %+v", s)
	}
}

// The API and /metrics refuse foreign host names (DNS rebinding) and
// every method but GET and HEAD, and an insecure configuration file is
// reported by the API and in /metrics.
func TestEndToEndHTTPHardening(t *testing.T) {
	port := freePort(t)
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(fmt.Sprintf(`{
	  "central_address": "1A2B3C",
	  "reset": "none",
	  "state_file": "",
	  "require_synced_clock": false,
	  "mqtt": {"broker": ""},
	  "http_listen": "127.0.0.1:%d",
	  "http_allowed_hosts": ["hmbridge.example"],
	  "devices": [{"name": "Bad", "model": "HM-CC-RT-DN", "address": "38F59C"}]
	}`, port)), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o644) // umask
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mod, rw := fakemod.New(fakemod.Options{})
	defer mod.Close()
	runCtx(t, cfg, rw)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))

	do := func(method, path, host string) (int, http.Header, string) {
		req, _ := http.NewRequest(method, base+path, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	get := func(path, host string) (int, string) {
		code, _, body := do("GET", path, host)
		return code, body
	}
	paths := []string{"/", "/api/v1/health", "/api/v1/bridge", "/api/v1/devices", "/api/v1/devices/38f59c", "/api/v1/groups", "/api/v1/unknown", "/api/state", "/metrics"}
	for _, p := range paths {
		if code, _ := get(p, fmt.Sprintf("evil.example:%d", port)); code != http.StatusMisdirectedRequest {
			t.Errorf("%s for evil.example: %d", p, code)
		}
		if code, _ := get(p, fmt.Sprintf("hmbridge.example:%d", port)); code != http.StatusOK {
			t.Errorf("%s for hmbridge.example: %d", p, code)
		}
		if code, _ := get(p, fmt.Sprintf("localhost:%d", port)); code != http.StatusOK {
			t.Errorf("%s for localhost: %d", p, code)
		}
		for _, m := range []string{"POST", "PUT", "DELETE"} {
			if code, h, _ := do(m, p, ""); code != http.StatusMethodNotAllowed || h.Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d, Allow %q", m, p, code, h.Get("Allow"))
			}
		}
	}
	if code, _, _ := do("POST", "/pairing/start", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /pairing/start: %d", code)
	}
	if code, idx := get("/", ""); code != http.StatusOK || !strings.Contains(idx, `"/api/v1/health"`) || strings.Contains(idx, "<html") {
		t.Errorf("index %d:\n%s", code, idx)
	}
	warning := path + " is readable by every user"
	for _, p := range []string{"/api/v1/health", "/api/v1/bridge"} {
		var v struct {
			Status   string   `json:"status"`
			Warnings []string `json:"warnings"`
		}
		_, body := get(p, "")
		if err := json.Unmarshal([]byte(body), &v); err != nil || len(v.Warnings) == 0 || !strings.Contains(v.Warnings[0], warning) {
			t.Errorf("%s lacks the warning:\n%s", p, body)
		}
		if p == "/api/v1/health" && v.Status != "ok" { // MQTT disabled, clock not required
			t.Errorf("health %s", body)
		}
	}
	if _, m := get("/metrics", ""); !strings.Contains(m, "hmbridge_config_insecure_permissions 1") {
		t.Error("/metrics lacks hmbridge_config_insecure_permissions 1")
	}
}
