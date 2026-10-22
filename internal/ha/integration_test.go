// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startMosquitto starts a private broker and returns its URL.
func startMosquitto(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("mosquitto")
	if err != nil {
		if _, serr := os.Stat("/usr/sbin/mosquitto"); serr != nil {
			t.Skip("mosquitto not installed")
		}
		bin = "/usr/sbin/mosquitto"
	}
	port := freePort(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "mosquitto.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command(bin, "-c", conf)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("mosquitto output:\n%s", out.String())
		}
	})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for deadline := time.Now().Add(5 * time.Second); ; {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mosquitto did not start: %v\n%s", err, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "tcp://" + addr
}

var observerSeq atomic.Int32

// observer is a second MQTT client recording the last message per topic.
type observer struct {
	mc mqtt.Client
	mu sync.Mutex
	m  map[string][]byte
}

func newObserver(t *testing.T, broker string) *observer {
	t.Helper()
	o := &observer{m: map[string][]byte{}}
	o.mc = mqtt.NewClient(mqtt.NewClientOptions().AddBroker(broker).SetClientID(fmt.Sprintf("observer-%d", observerSeq.Add(1))))
	if tok := o.mc.Connect(); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("observer connect: %v", tok.Error())
	}
	tok := o.mc.Subscribe("#", 1, func(_ mqtt.Client, m mqtt.Message) {
		o.mu.Lock()
		o.m[m.Topic()] = append([]byte(nil), m.Payload()...)
		o.mu.Unlock()
	})
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("observer subscribe: %v", tok.Error())
	}
	t.Cleanup(func() { o.mc.Disconnect(0) })
	return o
}

func (o *observer) wait(t *testing.T, topic, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.Lock()
		got, ok := o.m[topic]
		o.mu.Unlock()
		if ok && string(got) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic %s: got %q (present %v), want %q", topic, got, ok, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (o *observer) publish(t *testing.T, topic, payload string, retain bool) {
	t.Helper()
	if tok := o.mc.Publish(topic, 1, retain, payload); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("publish: %v", tok.Error())
	}
}

func waitCommand(t *testing.T, c *Client) Command {
	t.Helper()
	select {
	case cmd := <-c.Commands():
		return cmd
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for command")
	}
	panic("unreachable")
}

func TestIntegration(t *testing.T) {
	broker := startMosquitto(t)
	obs := newObserver(t, broker)

	dev := DeviceDesc{Addr: "38f59c", Name: "Wohnzimmer", Model: "HM-CC-RT-DN", Kind: RadiatorThermostat}
	grp := GroupDesc{Slug: "wohnzimmer", Name: "Wohnzimmer"}
	var onConnects, onConnectsDone atomic.Int32
	var c *Client
	ready := make(chan struct{})
	opts := Options{
		Broker:   broker,
		ClientID: "hmbridge-test",
		Logf:     t.Logf,
		OnConnect: func() {
			onConnects.Add(1)
			<-ready
			if err := c.PublishAll(DiscoveryMessages(c.Topics(), dev)); err != nil {
				t.Errorf("publishing discovery: %v", err)
			}
			if err := c.PublishAll(GroupDiscoveryMessages(c.Topics(), grp)); err != nil {
				t.Errorf("publishing group discovery: %v", err)
			}
			m, _ := StateMessage(c.Topics(), dev.Addr, State{TargetTemperature: ptr(21.0), Mode: "auto"})
			if err := c.Publish(m); err != nil {
				t.Errorf("publishing state: %v", err)
			}
			onConnectsDone.Add(1)
		},
	}
	stateBefore := testutil.ToFloat64(metricPublished.WithLabelValues("state"))
	birthBefore := testutil.ToFloat64(metricHABirth)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	c, err = Connect(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	defer c.Close()

	obs.wait(t, "hmbridge/bridge/availability", "online")
	obs.wait(t, "hmbridge/38f59c/state", `{"target_temperature":21,"mode":"auto"}`)
	obs.mu.Lock()
	_, okClimate := obs.m["homeassistant/climate/hmbridge_38f59c/config"]
	_, okGroup := obs.m["homeassistant/climate/hmbridge_group_wohnzimmer/config"]
	obs.mu.Unlock()
	if !okClimate || !okGroup {
		t.Errorf("discovery missing: device %v group %v", okClimate, okGroup)
	}
	if got := testutil.ToFloat64(metricPublished.WithLabelValues("state")); got < stateBefore+1 {
		t.Errorf("messages_published_total{kind=state} did not move")
	}
	if testutil.ToFloat64(metricConnected) != 1 {
		t.Error("connected gauge != 1")
	}

	// Retained availability is visible to late subscribers.
	late := newObserver(t, broker)
	late.wait(t, "hmbridge/bridge/availability", "online")

	// Command roundtrip (device and group); invalid ones are dropped.
	obs.publish(t, "hmbridge/38f59c/set/mode", "cool", false)
	obs.publish(t, "hmbridge/38f59c/set/temperature", "22.5", false)
	if cmd := waitCommand(t, c); cmd != (Command{Addr: "38f59c", Kind: SetTemperature, Temperature: 22.5}) {
		t.Errorf("got %+v", cmd)
	}
	obs.publish(t, "hmbridge/group/wohnzimmer/set/mode", "off", false)
	if cmd := waitCommand(t, c); cmd != (Command{Group: "wohnzimmer", Kind: SetMode, Mode: "off"}) {
		t.Errorf("got %+v", cmd)
	}

	// HA birth message triggers OnConnect again.
	n := onConnectsDone.Load()
	obs.publish(t, "homeassistant/status", "online", false)
	for deadline := time.Now().Add(5 * time.Second); onConnectsDone.Load() == n; {
		if time.Now().After(deadline) {
			t.Fatal("OnConnect not called after HA birth message")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if testutil.ToFloat64(metricHABirth) != birthBefore+1 {
		t.Error("ha_birth_messages_total did not move")
	}

	// Close publishes offline.
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	obs.wait(t, "hmbridge/bridge/availability", "offline")
	if _, ok := <-c.Commands(); ok {
		t.Error("Commands not closed")
	}
	if err := c.Publish(Message{Topic: "x"}); err == nil {
		t.Error("Publish after Close succeeded")
	}
}

// TestLWT kills the TCP connection under the client (unclean disconnect)
// via a proxy and checks that the broker publishes the "offline" LWT, and
// that Publish does not block while disconnected.
func TestLWT(t *testing.T) {
	broker := startMosquitto(t)
	obs := newObserver(t, broker)
	target := strings.TrimPrefix(broker, "tcp://")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accept := make(chan bool, 1) // false: refuse further connections
	accept <- true
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			ok := <-accept
			accept <- ok
			if !ok {
				in.Close()
				continue
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, in, out)
			mu.Unlock()
			go func() { defer out.Close(); bufCopy(out, in) }()
			go func() { defer in.Close(); bufCopy(in, out) }()
		}
	}()
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, Options{Broker: "tcp://" + l.Addr().String(), ClientID: "hmbridge-lwt", Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	obs.wait(t, "hmbridge/bridge/availability", "online")

	lossesBefore := testutil.ToFloat64(metricConnectionLosses)
	<-accept
	accept <- false
	mu.Lock()
	for _, conn := range conns {
		conn.Close()
	}
	mu.Unlock()
	obs.wait(t, "hmbridge/bridge/availability", "offline")

	for deadline := time.Now().Add(5 * time.Second); c.Connected(); {
		if time.Now().After(deadline) {
			t.Fatal("client still connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := c.Publish(Message{Topic: "hmbridge/38f59c/state", Payload: []byte("{}")}); err == nil {
		t.Error("Publish while disconnected succeeded")
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("Publish blocked for %v", d)
	}
	// paho runs the connection-lost handler (which counts) in its own
	// goroutine, after the client already reports itself disconnected.
	for deadline := time.Now().Add(5 * time.Second); testutil.ToFloat64(metricConnectionLosses) != lossesBefore+1; {
		if time.Now().After(deadline) {
			t.Errorf("connection_losses_total did not move (%v → %v)", lossesBefore, testutil.ToFloat64(metricConnectionLosses))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Allow reconnects: availability returns to online.
	<-accept
	accept <- true
	obs.wait(t, "hmbridge/bridge/availability", "online")
}

func bufCopy(dst, src net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func TestConnectCanceled(t *testing.T) {
	port := freePort(t) // nothing listens here
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Connect(ctx, Options{Broker: fmt.Sprintf("tcp://127.0.0.1:%d", port), Logf: t.Logf})
	if err == nil {
		t.Fatal("Connect succeeded")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Connect took %v after ctx deadline", d)
	}
}

func TestReadRetained(t *testing.T) {
	broker := startMosquitto(t)
	obs := newObserver(t, broker)
	cfg := TopicConfig{}
	obs.publish(t, "hmbridge/38f59c/state", `{"target_temperature":21}`, true)
	obs.publish(t, "hmbridge/group/bad/state", `{"window_open":true}`, true)
	obs.publish(t, "hmbridge/38f59c/availability", "online", true)    // not a state topic
	obs.publish(t, "hmbridge/2a1b3c/state", `{"humidity":50}`, false) // not retained

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, Options{Broker: broker, ClientID: "read-retained", Topics: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	got, err := c.ReadRetained(ctx, cfg.StateSubscriptions(), 200*time.Millisecond, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["hmbridge/38f59c/state"]) != `{"target_temperature":21}` || got["hmbridge/group/bad/state"] == nil {
		t.Errorf("ReadRetained = %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ReadRetained took %v; should return after the idle period", d)
	}
	for topic := range got {
		addr, group, ok := cfg.ParseStateTopic(topic)
		if !ok || (addr == "") == (group == "") {
			t.Errorf("ParseStateTopic(%s) = %q, %q, %v", topic, addr, group, ok)
		}
	}
	for _, bad := range []string{"hmbridge/38f59c/availability", "other/38f59c/state", "hmbridge/a/b/state", "hmbridge/group//state"} {
		if _, _, ok := cfg.ParseStateTopic(bad); ok {
			t.Errorf("ParseStateTopic(%s) accepted", bad)
		}
	}
}
