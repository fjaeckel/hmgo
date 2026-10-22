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
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"

	bidcosaes "github.com/fjaeckel/hmgo/internal/bidcos/aes"
	"github.com/fjaeckel/hmgo/internal/bridge"
	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/gpio"
	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/serial"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

var (
	configPath  = flag.String("config", "/etc/hmbridge/config.json", "path to the JSON configuration")
	checkConfig = flag.Bool("check_config", false, "validate the configuration and exit")
	debugFrames = flag.Bool("debug", false, "log every frame exchanged with the radio module (keys are redacted)")
	showVersion = flag.Bool("version", false, "print the version and exit")
)

func version() string {
	v := "devel"
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				v += "+" + s.Value[:12]
			}
		}
	}
	return v
}

var buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "hmbridge", Name: "build_info",
	Help: "Build information (always 1).",
}, []string{"version", "goversion"})

func init() { prometheus.MustRegister(buildInfo) }

func main() {
	flag.Parse()
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
	conn, err := openModule(cfg, uart)
	if err != nil {
		uart.Close()
		return err
	}
	defer conn.Close()
	log.Printf("radio module ready: %s, firmware %s, serial %s, central address %x", conn.Kind(), conn.FirmwareVersion(), conn.SerialNumber(), cfg.CentralAddr())
	if conn.Kind() == uartgw.KindRPIRFMod {
		log.Printf("RPI-RF-MOD support is experimental: acknowledgements and AES are done in software, and parts of the module protocol are still unverified (see docs/SNIFFING.md)")
	}

	b, err := bridge.New(bridge.Options{Config: cfg, Radio: conn})
	if err != nil {
		return err
	}
	if err := b.Setup(ctx); err != nil {
		return err
	}

	if addr := cfg.HTTPListenAddr(); addr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.Handle("/", b.StatusHandler(bridge.Info{
			"version":  version(),
			"module":   conn.Kind(),
			"firmware": conn.FirmwareVersion(),
			"serial":   conn.SerialNumber(),
		}))
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
func openModule(cfg *config.Config, uart io.ReadWriter) (uartgw.Module, error) {
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
