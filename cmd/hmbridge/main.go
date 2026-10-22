// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

// Command hmbridge bridges classic HomeMatic (BidCoS-RF) thermostats and
// window contacts to Home Assistant via MQTT, using an HM-MOD-RPI-PCB
// radio module on a Raspberry Pi instead of a CCU.
//
// See docs/DEPLOY.md for setup and internal/config/example.json for the
// configuration format.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/bridge"
	"github.com/fjaeckel/hmgo/internal/buildinfo"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/gpio"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/serial"
	"github.com/fjaeckel/hmgo/internal/store"
	"github.com/fjaeckel/hmgo/internal/uartgw"
	"github.com/fjaeckel/hmgo/licenses"
)

var (
	configPath  = flag.String("config", "/etc/hmbridge/config.json", "path to the JSON configuration")
	checkConfig = flag.Bool("check_config", false, "validate the configuration and exit")
	debugFrames = flag.Bool("debug", false, "log every frame exchanged with the radio module (keys are redacted) and every message hmbridge does not use")
	showVersion = flag.Bool("version", false, "print the version and exit")
	showLicense = flag.Bool("license", false, "print the licenses and exit")
)

func version() string { return buildinfo.Version() }

var buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "hmbridge", Name: "build_info",
	Help: "Build information (always 1).",
}, []string{"version", "goversion"})

var configInsecure = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "hmbridge", Name: "config_insecure_permissions",
	Help: "1 if the configuration file (AES system key, MQTT password) is readable by other users or owned by another user (see the log).",
})

func init() { prometheus.MustRegister(buildInfo, configInsecure) }

func main() {
	flag.Parse()
	if *showLicense {
		fmt.Print(licenses.Text())
		return
	}
	if *showVersion {
		fmt.Println("hmbridge", version(), runtime.Version())
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if *checkConfig {
		fmt.Printf("%s: OK (%d devices, %d groups)\n", *configPath, len(cfg.Devices), len(cfg.Groups))
		for _, m := range cfg.Deprecations() {
			fmt.Printf("warning: %s\n", m)
		}
		for _, m := range cfg.PermissionWarnings() {
			fmt.Printf("WARNING: %s\n", m)
		}
		if d := cfg.StateDirPath(); d != "" {
			fmt.Printf("state directory: %s\n", d)
		}
		if p := cfg.DevicesFilePath(); p != "" {
			fmt.Printf("paired devices are read from %s (pairing AES: %s)\n", p, cfg.PairingAES())
		}
		return
	}
	log.Printf("hmbridge %s (%s) starting, %d devices, %d groups", version(), runtime.Version(), len(cfg.Devices), len(cfg.Groups))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	uart, err := openUART(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := run(ctx, cfg, uart); err != nil && !errors.Is(err, context.Canceled) {
		// Exit non-zero so that systemd restarts us; the module is
		// re-initialised on the next start.
		log.Fatal(err)
	}
	log.Printf("shut down")
}

// run runs hmbridge on an opened radio module connection until ctx is
// done or the radio fails. uart is closed on return.
func run(ctx context.Context, cfg *config.Config, uart io.ReadWriteCloser) error {
	buildInfo.WithLabelValues(version(), runtime.Version()).Set(1)
	var debugf func(string, ...any)
	if *debugFrames {
		debugf = log.Printf
	}
	// Homematic IP frames of the RPI-RF-MOD: shown, not supported yet.
	hmipMon := bridge.NewHMIPMonitor(nil, debugf)
	conn, err := openModule(cfg, uart, hmipMon.Handle)
	if err != nil {
		uart.Close()
		return err
	}
	defer conn.Close()
	log.Printf("radio module ready: %s, firmware %s, serial %s, central address %x", conn.Kind(), conn.FirmwareVersion(), conn.SerialNumber(), cfg.CentralAddr())
	if conn.Kind() == uartgw.KindRPIRFMod {
		log.Printf("RPI-RF-MOD support is experimental: acknowledgements and AES are done in software, and parts of the module protocol are still unverified (see docs/SNIFFING.md)")
	}

	for _, m := range cfg.Deprecations() {
		log.Printf("config: %s", m)
	}
	permWarnings := cfg.PermissionWarnings()
	for _, m := range permWarnings {
		log.Printf("WARNING: config: %s", m)
	}
	configInsecure.Set(boolGauge(len(permWarnings) > 0))
	st := store.OpenConfig(cfg, log.Printf)
	if d := st.Dir(); d != "" {
		log.Printf("state directory: %s", d)
	} else {
		log.Printf("state_dir is \"\": no state file, pairing disabled")
	}
	opts := bridge.Options{Config: cfg, Radio: conn, Debugf: debugf, HMIP: hmipMon, Version: version(), Store: st}
	if testBridgeOptions != nil {
		testBridgeOptions(&opts)
	}
	b, err := bridge.New(opts)
	if err != nil {
		return err
	}
	if err := b.Setup(ctx); err != nil {
		return err
	}

	if addr := cfg.HTTPListenAddr(); addr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.Handle("/", b.StatusPage(bridge.StatusOptions{
			Info: bridge.Info{
				"version":  version(),
				"module":   conn.Kind(),
				"firmware": conn.FirmwareVersion(),
				"serial":   conn.SerialNumber(),
			},
			Warnings: permWarnings,
		}))
		// Every request (status page, JSON, /metrics) must name hmbridge
		// in its Host header (DNS rebinding).
		srv := newHTTPServer(bridge.NewHostAllowlist(addr, cfg.HTTPAllowedHosts).Handler(mux))
		// Listen before serving in the background: an address that
		// cannot be bound is a startup error, not a log line.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("http_listen: %w", err)
		}
		ln = limitListener(ln, httpMaxConns)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("HTTP server: %v", err)
			}
		}()
		defer srv.Close()
		log.Printf("status page and metrics on %s", addr)
	}

	if broker := cfg.MQTT.BrokerURL(); broker != "" {
		mqttCtx, cancelMQTT := context.WithCancel(ctx)
		mqttDone := make(chan struct{})
		// On shutdown, wait for the client to publish “offline”.
		defer func() {
			cancelMQTT()
			<-mqttDone
		}()
		// Connect in the background: the radio side keeps working (and
		// the devices keep being acknowledged) while the broker is down.
		go func() {
			defer close(mqttDone)
			client, err := connectMQTT(mqttCtx, ha.Options{
				Broker:    broker,
				Username:  cfg.MQTT.Username,
				Password:  cfg.MQTT.Password,
				ClientID:  cfg.MQTT.ClientID,
				Topics:    ha.TopicConfig{Base: cfg.MQTT.BaseTopic, DiscoveryPrefix: cfg.MQTT.DiscoveryPrefix},
				OnConnect: b.PublishAll,
			})
			if err != nil {
				return // only on shutdown
			}
			log.Printf("MQTT connected to %s", broker)
			// Restore the last published state from the broker's
			// retained messages before publishing anything, so that a
			// restart does not blank Home Assistant (and costs no SD
			// card writes).
			retained, err := client.ReadRetained(mqttCtx, client.Topics().StateSubscriptions(), 0, 0)
			if err != nil && mqttCtx.Err() == nil {
				log.Printf("MQTT: reading retained state: %v", err)
			}
			b.SeedFromRetained(client.Topics(), retained)
			b.SetPublisher(client)
			go func() {
				for cmd := range client.Commands() {
					select {
					case b.Commands() <- cmd:
					case <-mqttCtx.Done():
						return
					}
				}
			}()
			<-mqttCtx.Done()
			b.SetPublisher(nil)
			client.Close()
		}()
	} else {
		log.Printf("MQTT disabled (empty mqtt.broker)")
	}

	return b.Run(ctx)
}

// openModule initialises the radio module selected by cfg.Radio ("auto"
// detects it).
// testBridgeOptions lets tests shorten the bridge's delays (retries,
// status polls). Always nil outside tests.
var testBridgeOptions func(*bridge.Options)

func openModule(cfg *config.Config, uart io.ReadWriter, hmipHandler func(dst, cnt byte, packet []byte)) (uartgw.Module, error) {
	current, previous := cfg.Keys()
	keys := []hmkey.Key{*current}
	if previous != nil {
		keys = append(keys, *previous)
	}
	ucfg := uartgw.Config{
		HMID:     cfg.CentralAddr(),
		Current:  current,
		Previous: previous,
		CSMACA:   cfg.CSMACA,
		// Don't hand a stale clock to the module (no RTC on a Pi); the
		// bridge sets it once NTP has synchronised.
		SkipSetTime: cfg.RequiresSyncedClock() && !bridge.SystemClockSynced(),
		// The RPI-RF-MOD leaves AES to the host.
		Signer: bidcosaes.NewSigner(keys...),
		Debug:  *debugFrames,
		// RPI-RF-MOD only; receive only.
		HMIPHandler: hmipHandler,
	}
	var (
		mod uartgw.Module
		err error
	)
	switch cfg.Radio {
	case config.RadioHMModRPIPCB:
		var c *uartgw.Conn
		if c, err = uartgw.Open(uart, ucfg); err == nil {
			mod = c
		}
	case config.RadioRPIRFMod:
		var c *uartgw.DualConn
		if c, err = uartgw.OpenDual(uart, ucfg); err == nil {
			mod = c
		}
	default:
		mod, err = uartgw.OpenModule(uart, ucfg)
	}
	if err != nil {
		if errors.Is(err, uartgw.ErrDualCoProFirmware) {
			return nil, fmt.Errorf("%w\n\nThe radio module is an RPI-RF-MOD (or an HM-MOD-RPI-PCB with eQ-3's dual firmware), but the configuration says \"radio\": %q. Set \"radio\": \"auto\" or \"rpi-rf-mod\"", err, cfg.Radio)
		}
		return nil, fmt.Errorf("initialising the radio module on %s: %w", cfg.SerialPort, err)
	}
	return mod, nil
}

// mqttAuthRetry is the delay between attempts after the broker refused
// the credentials. The Mosquitto add-on of Home Assistant refuses logins
// while Home Assistant itself is still starting (e.g. after a power
// outage, when the Pi is up first), so this is not treated as fatal.
const mqttAuthRetry = time.Minute

// connectMQTT connects to the broker, retrying until it succeeds or ctx
// is done. ha.Connect itself retries network errors; rejected
// credentials are retried here with a longer delay.
func connectMQTT(ctx context.Context, opts ha.Options) (*ha.Client, error) {
	for {
		client, err := ha.Connect(ctx, opts)
		if err == nil {
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Printf("MQTT: %v; retrying in %v (check mqtt.username/password if this persists)", err, mqttAuthRetry)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(mqttAuthRetry):
		}
	}
}

// openUART opens and configures the serial port and resets the module
// if configured.
func openUART(cfg *config.Config) (*os.File, error) {
	log.Printf("opening serial port %s", cfg.SerialPort)
	uart, err := os.OpenFile(cfg.SerialPort, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	// Use SyscallConn instead of Fd: Fd would switch the file to
	// blocking mode, after which Close could not interrupt a pending read.
	rc, err := uart.SyscallConn()
	if err != nil {
		uart.Close()
		return nil, err
	}
	var ctlErr error
	if err := rc.Control(func(fd uintptr) {
		if serial.IsTerminal(fd) {
			if err := serial.Configure(fd); err != nil {
				ctlErr = fmt.Errorf("configuring %s: %w", cfg.SerialPort, err)
				return
			}
		} else {
			log.Printf("%s is not a tty (e.g. OpenCCU's raw-uart), skipping line configuration", cfg.SerialPort)
		}
		switch cfg.Reset {
		case "gpio":
			log.Printf("resetting HM-MOD-RPI-PCB via GPIO %d", cfg.ResetGPIO)
			if err := gpio.ResetUARTGW(fd, cfg.ResetGPIO); err != nil {
				ctlErr = fmt.Errorf("resetting the radio module via GPIO %d: %w (set \"reset\": \"none\" if the module's reset line is not connected)", cfg.ResetGPIO, err)
			}
		case "raw-uart":
			log.Printf("resetting the radio module via the raw-uart driver")
			if err := serial.ResetRawUART(fd); err != nil {
				hint := ""
				if errors.Is(err, syscall.EBUSY) {
					hint = " (another program uses the radio module; on OpenCCU stop multimacd first, see docs/DEPLOY.md)"
				}
				ctlErr = fmt.Errorf("resetting the radio module via %s: %w%s", cfg.SerialPort, err, hint)
			}
		}
	}); err != nil {
		ctlErr = err
	}
	if ctlErr != nil {
		uart.Close()
		return nil, ctlErr
	}
	return uart, nil
}

func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
