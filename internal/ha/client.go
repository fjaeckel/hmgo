package ha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Availability payloads.
const (
	PayloadOnline  = "online"
	PayloadOffline = "offline"
)

// PublishTimeout bounds how long Publish waits for the broker.
const PublishTimeout = time.Second

// ErrNotConnected is returned by Publish while the broker is unreachable.
var ErrNotConnected = errors.New("ha: not connected to MQTT broker")

// Options configure a Client.
type Options struct {
	Broker   string // e.g. "tcp://homeassistant.local:1883"
	Username string
	Password string
	ClientID string // default "hmbridge"
	Topics   TopicConfig

	// OnConnect is called after every (re)connect and whenever Home
	// Assistant announces itself on <prefix>/status. The bridge should
	// publish all DiscoveryMessages and the current StateMessage of every
	// device. Calls are serialized (never concurrent) and coalesced; they
	// run on a goroutine of the Client, so OnConnect may call Publish.
	OnConnect func()

	// Logf logs errors (dropped commands, publish failures in callbacks,
	// connection loss). Default log.Printf.
	Logf func(format string, args ...any)
}

// Client is a connection to the MQTT broker. Its methods are safe for
// concurrent use.
type Client struct {
	opts    Options
	cfg     TopicConfig
	mc      mqtt.Client
	logf    func(string, ...any)
	trigger chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	commands chan Command
}

// Connect connects to the broker, retrying until the first connection
// succeeds or ctx is done (in which case the last connection error is
// returned). Rejected credentials fail immediately. After the first
// connection the client reconnects automatically on its own; ctx only
// governs the initial connection.
//
// The availability topic carries a retained "offline" LWT.
func Connect(ctx context.Context, o Options) (*Client, error) {
	if o.Broker == "" {
		return nil, errors.New("ha: no MQTT broker configured")
	}
	if o.ClientID == "" {
		o.ClientID = "hmbridge"
	}
	c := &Client{
		opts:     o,
		cfg:      o.Topics,
		logf:     o.Logf,
		trigger:  make(chan struct{}, 1),
		done:     make(chan struct{}),
		commands: make(chan Command, 64),
	}
	if c.logf == nil {
		c.logf = log.Printf
	}

	mo := mqtt.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(o.ClientID).
		SetUsername(o.Username).
		SetPassword(o.Password).
		SetCleanSession(true).
		SetStore(nullStore{}).
		SetAutoReconnect(true).
		SetConnectRetry(false).
		SetMaxReconnectInterval(30*time.Second).
		SetConnectTimeout(10*time.Second).
		SetWriteTimeout(PublishTimeout).
		SetKeepAlive(30*time.Second).
		SetWill(c.cfg.AvailabilityTopic(), PayloadOffline, 1, true).
		SetOnConnectHandler(func(mqtt.Client) { c.onConnect() }).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			metricConnected.Set(0)
			metricConnectionLosses.Inc()
			c.logf("ha: MQTT connection lost: %v (reconnecting)", err)
		})
	c.mc = mqtt.NewClient(mo)

	c.wg.Add(1)
	go c.worker()

	backoff := time.Second
	for {
		metricConnectAttempts.Inc()
		tok := c.mc.Connect()
		var err error
		select {
		case <-tok.Done():
			err = tok.Error()
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err == nil {
			return c, nil
		}
		if isAuthError(err) {
			c.shutdown()
			return nil, fmt.Errorf("ha: connecting to %s: %w", o.Broker, err)
		}
		if ctx.Err() != nil {
			c.mc.Disconnect(0)
			c.shutdown()
			return nil, fmt.Errorf("ha: connecting to %s: %w", o.Broker, err)
		}
		c.logf("ha: connecting to %s: %v (retrying in %v)", o.Broker, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.shutdown()
			return nil, fmt.Errorf("ha: connecting to %s: %w (last error: %v)", o.Broker, ctx.Err(), err)
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// isAuthError reports whether the broker refused the connection in a way
// that retrying will not fix.
func isAuthError(err error) bool {
	return errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) ||
		errors.Is(err, packets.ErrorRefusedNotAuthorised) ||
		errors.Is(err, packets.ErrorRefusedIDRejected) ||
		errors.Is(err, packets.ErrorRefusedBadProtocolVersion)
}

// onConnect runs on a paho goroutine after every successful connect.
func (c *Client) onConnect() {
	metricConnected.Set(1)
	if err := c.publish(Message{Topic: c.cfg.AvailabilityTopic(), Payload: []byte(PayloadOnline), Retain: true, QoS: 1}); err != nil {
		c.logf("ha: publishing availability: %v", err)
	}
	subs := map[string]byte{
		c.cfg.CommandSubscription():      1,
		c.cfg.GroupCommandSubscription(): 1,
		c.cfg.HAStatusTopic():            1,
	}
	tok := c.mc.SubscribeMultiple(subs, c.handleMessage)
	if !tok.WaitTimeout(5 * time.Second) {
		c.logf("ha: subscribing: timeout")
	} else if err := tok.Error(); err != nil {
		c.logf("ha: subscribing: %v", err)
	}
	c.kick()
}

func (c *Client) kick() {
	select {
	case c.trigger <- struct{}{}:
	default: // already pending: coalesce
	}
}

func (c *Client) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case <-c.trigger:
			if c.opts.OnConnect != nil {
				c.opts.OnConnect()
			}
		}
	}
}

// handleMessage runs on paho's router goroutine and must not block.
func (c *Client) handleMessage(_ mqtt.Client, m mqtt.Message) {
	if m.Topic() == c.cfg.HAStatusTopic() {
		if string(m.Payload()) == PayloadOnline {
			metricHABirth.Inc()
			c.kick()
		}
		return
	}
	if m.Retained() {
		// Stale retained commands (e.g. published by mistake with the
		// retain flag) must not be replayed on every reconnect.
		metricInvalidCommands.Inc()
		c.logf("ha: ignoring retained command on %s", m.Topic())
		return
	}
	cmd, err := ParseCommand(c.cfg, m.Topic(), m.Payload())
	if err != nil {
		metricInvalidCommands.Inc()
		c.logf("ha: dropping command on %s (%q): %v", m.Topic(), m.Payload(), err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.commands <- cmd:
		metricCommands.WithLabelValues(cmd.Kind.metricLabel()).Inc()
	default:
		metricInvalidCommands.Inc()
		c.logf("ha: command queue full, dropping %+v", cmd)
	}
}

// Commands returns the channel of validated commands from HA. It is closed
// by Close.
func (c *Client) Commands() <-chan Command { return c.commands }

// Topics returns the topic configuration.
func (c *Client) Topics() TopicConfig { return c.cfg }

// Connected reports whether the connection to the broker is currently up.
func (c *Client) Connected() bool { return c.mc.IsConnectionOpen() }

// Publish publishes m and waits at most PublishTimeout for the broker's
// acknowledgement. While disconnected it fails fast with ErrNotConnected
// (nothing is queued; OnConnect re-publishes everything after reconnect).
func (c *Client) Publish(m Message) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("ha: client closed")
	}
	return c.publish(m)
}

// PublishAll publishes msgs and returns the first error (it keeps going on
// errors so that one bad message does not block the rest).
func (c *Client) PublishAll(msgs []Message) error {
	var first error
	for _, m := range msgs {
		if err := c.Publish(m); err != nil && first == nil {
			first = err
			if errors.Is(err, ErrNotConnected) {
				return err
			}
		}
	}
	return first
}

func (c *Client) publish(m Message) error {
	err := c.publishRaw(m)
	if err != nil {
		metricPublishErrors.Inc()
	} else {
		metricPublished.WithLabelValues(c.cfg.messageKind(m.Topic)).Inc()
	}
	return err
}

func (c *Client) publishRaw(m Message) error {
	if !c.mc.IsConnectionOpen() {
		return ErrNotConnected
	}
	payload := m.Payload
	if payload == nil {
		payload = []byte{}
	}
	tok := c.mc.Publish(m.Topic, m.QoS, m.Retain, payload)
	if !tok.WaitTimeout(PublishTimeout) {
		return fmt.Errorf("ha: publishing to %s: timeout", m.Topic)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("ha: publishing to %s: %w", m.Topic, err)
	}
	return nil
}

// Close publishes availability "offline", disconnects and closes the
// Commands channel. It waits for a running OnConnect callback to return,
// so it must not be called from within OnConnect.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.commands)
	c.mu.Unlock()

	err := c.publish(Message{Topic: c.cfg.AvailabilityTopic(), Payload: []byte(PayloadOffline), Retain: true, QoS: 1})
	if errors.Is(err, ErrNotConnected) {
		err = nil // the broker publishes the LWT
	}
	c.mc.Disconnect(250)
	metricConnected.Set(0)
	c.shutdown()
	return err
}

func (c *Client) shutdown() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.wg.Wait()
}

// nullStore is an mqtt.Store that stores nothing. With a clean session
// and OnConnect re-publishing everything, resending stored messages after
// a reconnect is unnecessary; paho's resume of stored publishes also races
// with its own outgoing writer (data race on PublishPacket.Dup).
type nullStore struct{}

func (nullStore) Open()                             {}
func (nullStore) Put(string, packets.ControlPacket) {}
func (nullStore) Get(string) packets.ControlPacket  { return nil }
func (nullStore) All() []string                     { return nil }
func (nullStore) Del(string)                        {}
func (nullStore) Close()                            {}
func (nullStore) Reset()                            {}
