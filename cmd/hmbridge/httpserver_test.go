// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
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

// The status page, the JSON API and /metrics refuse foreign host names
// (DNS rebinding), and an insecure configuration file is reported on
// the status page and in /metrics.
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

	get := func(path, host string) (int, string) {
		req, _ := http.NewRequest("GET", base+path, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, p := range []string{"/", "/api/state", "/metrics"} {
		if code, _ := get(p, fmt.Sprintf("evil.example:%d", port)); code != http.StatusMisdirectedRequest {
			t.Errorf("%s for evil.example: %d", p, code)
		}
		if code, _ := get(p, fmt.Sprintf("hmbridge.example:%d", port)); code != http.StatusOK {
			t.Errorf("%s for hmbridge.example: %d", p, code)
		}
		if code, _ := get(p, fmt.Sprintf("localhost:%d", port)); code != http.StatusOK {
			t.Errorf("%s for localhost: %d", p, code)
		}
	}
	if code, page := get("/", ""); code != http.StatusOK || !strings.Contains(page, "WARNING: "+path+" is readable by every user") {
		t.Errorf("status page %d:\n%s", code, page)
	}
	if _, m := get("/metrics", ""); !strings.Contains(m, "hmbridge_config_insecure_permissions 1") {
		t.Error("/metrics lacks hmbridge_config_insecure_permissions 1")
	}
	if _, js := get("/api/state", ""); !strings.Contains(js, `"warnings": [`) {
		t.Errorf("/api/state lacks the warnings:\n%s", js)
	}
}
