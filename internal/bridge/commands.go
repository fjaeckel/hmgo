package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
	"github.com/fjaeckel/hmgo/internal/hm"
	"github.com/fjaeckel/hmgo/internal/hm/heating"
	"github.com/fjaeckel/hmgo/internal/hm/thermal"
	"github.com/fjaeckel/hmgo/internal/uartgw"
)

// DefaultComfortTemperature is used when switching a device to heat
// mode while its last non-off target temperature is unknown.
const DefaultComfortTemperature = 20.0

// jobTimeout bounds a single radio job. A burst command with retries
// takes a few seconds; the radio layer has its own, shorter timeouts.
const jobTimeout = 30 * time.Second

type jobKind int

const (
	jobClimate jobKind = iota
	jobTimeResponse
	jobSetTime
)

type climateAction string

const (
	actionSetTemperature climateAction = "set_temperature"
	actionAuto           climateAction = "auto"
	actionManual         climateAction = "manual"
	actionBoost          climateAction = "boost"
)

type job struct {
	key string // coalescing key: a newer job with the same key replaces an older queued one
	// target groups jobs whose retries a newer job supersedes (all
	// climate jobs of one device). Empty for jobs that are never retried.
	target string
	seq    uint64 // assigned by the queue
	kind   jobKind
	dev    *device
	// priority jobs (time responses) jump the queue: a device listens
	// for the answer only briefly.
	priority bool

	action  climateAction
	temp    float64
	attempt int

	reqNo byte // jobTimeResponse: message counter of the request
}

type jobResult struct {
	job job
	ev  any // device response event (e.g. *heating.AckStatusEvent), may be nil
	err error
}

// jobQueue is a FIFO queue of jobs that coalesces jobs with the same key:
// pushing a job whose key is already queued replaces the queued job and
// moves it to the back, so the most recent command always runs last.
// This keeps a slider being dragged in Home Assistant from queueing
// dozens of radio transmissions.
//
// Retries: a failed climate job may be retried later, but only if no
// newer job for the same target (device) was pushed in the meantime —
// of any kind. Otherwise a retried “22 °C” could undo a later “off”.
type jobQueue struct {
	mu         sync.Mutex
	order      []string
	jobs       map[string]job
	lastTarget map[string]uint64 // target → seq of the newest pushed job
	seq        uint64
	signal     chan struct{}
}

func newJobQueue() *jobQueue {
	return &jobQueue{
		jobs:       make(map[string]job),
		lastTarget: make(map[string]uint64),
		signal:     make(chan struct{}, 1),
	}
}

// insert adds j, replacing a queued job with the same key. Caller holds q.mu.
func (q *jobQueue) insert(j job) {
	if _, queued := q.jobs[j.key]; queued {
		for i, k := range q.order {
			if k == j.key {
				q.order = append(q.order[:i], q.order[i+1:]...)
				break
			}
		}
	}
	if j.priority {
		q.order = append([]string{j.key}, q.order...)
	} else {
		q.order = append(q.order, j.key)
	}
	q.jobs[j.key] = j
	metricQueueLength.Set(float64(len(q.order)))
}

func (q *jobQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// push enqueues j and returns its sequence number.
func (q *jobQueue) push(j job) uint64 {
	q.mu.Lock()
	q.seq++
	j.seq = q.seq
	if j.target != "" {
		q.lastTarget[j.target] = j.seq
	}
	q.insert(j)
	q.mu.Unlock()
	q.wake()
	return j.seq
}

// retry re-enqueues j unless a newer job for the same target was pushed
// in the meantime (which supersedes the retry).
func (q *jobQueue) retry(j job) bool {
	q.mu.Lock()
	// Check and re-insert under one lock, so that a newer job pushed
	// concurrently can never be replaced by this stale retry.
	if j.target == "" || q.lastTarget[j.target] != j.seq {
		q.mu.Unlock()
		return false
	}
	q.insert(j)
	q.mu.Unlock()
	q.wake()
	return true
}

func (q *jobQueue) pop() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return job{}, false
	}
	key := q.order[0]
	q.order = q.order[1:]
	j := q.jobs[key]
	delete(q.jobs, key)
	metricQueueLength.Set(float64(len(q.order)))
	return j, true
}

func (q *jobQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// worker executes queued jobs one at a time.
func (b *Bridge) worker(ctx context.Context) {
	for {
		j, ok := b.queue.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-b.queue.signal:
				continue
			}
		}
		jctx, cancel := context.WithTimeout(ctx, jobTimeout)
		ev, err := b.execute(jctx, j)
		cancel()
		select {
		case b.results <- jobResult{job: j, ev: ev, err: err}:
		case <-ctx.Done():
			return
		}
	}
}

func (b *Bridge) execute(ctx context.Context, j job) (any, error) {
	switch j.kind {
	case jobSetTime:
		if !b.timeUsable() {
			return nil, nil
		}
		return nil, b.radio.SetTime(ctx, b.now())

	case jobTimeResponse:
		if !b.timeUsable() {
			return nil, nil
		}
		pkt := hm.TimestampResponse(j.reqNo, b.cfg.CentralAddr(), j.dev.cfg.Addr(), b.now())
		_, err := b.bcs.Send(ctx, pkt)
		return nil, err

	case jobClimate:
		switch dev := j.dev.dev.(type) {
		case *heating.Thermostat:
			return climateCall(ctx, j, dev.SetTemperature, dev.SetAutoMode, dev.SetManualMode, dev.SetBoost)
		case *thermal.ThermalControl:
			return climateCall(ctx, j, dev.SetTemperature, dev.SetAutoMode, dev.SetManualMode, dev.SetBoost)
		default:
			return nil, fmt.Errorf("%s does not accept climate commands", j.dev.cfg.Name)
		}
	}
	return nil, fmt.Errorf("unknown job kind %d", j.kind)
}

// climateCall dispatches a climate job to the device's command methods.
// It is generic over the device's ACK status event type.
func climateCall[E any](ctx context.Context, j job,
	setTemp func(context.Context, float64) (E, error),
	auto func(context.Context) (E, error),
	manual func(context.Context, float64) (E, error),
	boost func(context.Context) (E, error),
) (any, error) {
	var (
		ev  E
		err error
	)
	switch j.action {
	case actionSetTemperature:
		ev, err = setTemp(ctx, j.temp)
	case actionAuto:
		ev, err = auto(ctx)
	case actionManual:
		ev, err = manual(ctx, j.temp)
	case actionBoost:
		ev, err = boost(ctx)
	default:
		return nil, fmt.Errorf("unknown climate action %q", j.action)
	}
	if err != nil {
		return nil, err
	}
	// A plain ACK yields a typed nil pointer; don't wrap it into a
	// non-nil interface value.
	var zero E
	if any(ev) == any(zero) {
		return nil, nil
	}
	return ev, nil
}

// handleCommand translates a Home Assistant command into radio jobs.
// Runs on the event loop.
func (b *Bridge) handleCommand(cmd ha.Command) {
	var targets []*device
	var what string
	switch {
	case cmd.Group != "":
		g, ok := b.bySlug[cmd.Group]
		if !ok {
			b.logf("command for unknown group %q ignored", cmd.Group)
			metricCommands.WithLabelValues("unknown_target", "rejected").Inc()
			return
		}
		targets = g.targets
		what = "group " + g.name
	default:
		var d *device
		for _, cand := range b.devices {
			if cand.desc.Addr == cmd.Addr {
				d = cand
			}
		}
		if d == nil {
			b.logf("command for unknown device %q ignored", cmd.Addr)
			metricCommands.WithLabelValues("unknown_target", "rejected").Inc()
			return
		}
		if d.desc.Kind != ha.RadiatorThermostat && d.desc.Kind != ha.WallThermostat {
			b.logf("command for %s ignored: not a thermostat", d.cfg.Name)
			metricCommands.WithLabelValues("not_thermostat", "rejected").Inc()
			return
		}
		targets = []*device{d}
		what = d.cfg.Name
	}

	for _, d := range targets {
		action, temp, err := b.translate(d, cmd)
		if errors.Is(err, errNoop) {
			continue
		}
		if err != nil {
			b.logf("command for %s: %v", what, err)
			metricCommands.WithLabelValues(string(action), "rejected").Inc()
			continue
		}
		b.logf("%s: queueing %s %v (%s)", d.cfg.Name, action, temp, what)
		b.queue.push(job{
			key:    fmt.Sprintf("climate:%s:%s", d.desc.Addr, actionClass(action)),
			target: "climate:" + d.desc.Addr,
			kind:   jobClimate,
			dev:    d,
			action: action,
			temp:   temp,
		})
	}
}

// actionClass groups actions that override each other for coalescing:
// a mode change supersedes a queued mode change, a temperature change a
// queued temperature change.
func actionClass(a climateAction) string {
	if a == actionSetTemperature {
		return "temperature"
	}
	return "mode"
}

// translate maps an HA command to a device action.
func (b *Bridge) translate(d *device, cmd ha.Command) (climateAction, float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch cmd.Kind {
	case ha.SetTemperature:
		return actionSetTemperature, cmd.Temperature, nil
	case ha.SetMode:
		switch cmd.Mode {
		case ha.ModeAuto:
			return actionAuto, 0, nil
		case ha.ModeHeat:
			return actionManual, comfortTarget(d), nil
		case ha.ModeOff:
			return actionManual, ha.MinTemp, nil
		}
		return "", 0, fmt.Errorf("unsupported mode %q", cmd.Mode)
	case ha.SetPreset:
		switch cmd.Preset {
		case ha.PresetBoost:
			return actionBoost, 0, nil
		case ha.PresetNone:
			if d.state.ControlMode != "boost" {
				return "", 0, errNoop
			}
			// Leaving boost: the devices have no “end boost” command, so
			// return to the mode HA showed before boost.
			switch d.preBoostHAMode {
			case ha.ModeOff:
				return actionManual, ha.MinTemp, nil
			case ha.ModeHeat:
				return actionManual, comfortTarget(d), nil
			}
			return actionAuto, 0, nil
		}
		return "", 0, fmt.Errorf("unsupported preset %q", cmd.Preset)
	}
	return "", 0, fmt.Errorf("unsupported command kind %v", cmd.Kind)
}

// errNoop is returned by translate for commands that need no radio
// transmission (e.g. preset none while not boosting).
var errNoop = errors.New("nothing to do")

// comfortTarget returns the temperature to use when switching to heat.
func comfortTarget(d *device) float64 {
	if d.lastOnTarget > ha.MinTemp {
		return d.lastOnTarget
	}
	if t := d.state.TargetTemperature; t != nil && *t > ha.MinTemp && *t < ha.MaxTemp {
		return *t
	}
	return DefaultComfortTemperature
}

// handleResult applies the outcome of a job. Runs on the event loop.
func (b *Bridge) handleResult(res jobResult) {
	j := res.job
	switch j.kind {
	case jobSetTime:
		if res.err != nil {
			b.logf("setting radio module time: %v", res.err)
		}
		return
	case jobTimeResponse:
		if res.err != nil {
			b.logf("answering time request of %s: %v", j.dev.cfg.Name, res.err)
		}
		return
	}

	d := j.dev
	if res.err != nil {
		metricCommands.WithLabelValues(string(j.action), "error").Inc()
		if isRetryable(res.err) && j.attempt < b.retries {
			j.attempt++
			b.logf("%s: %s not acknowledged (%v), retrying in %v (attempt %d of %d)", d.cfg.Name, j.action, res.err, b.retryDelay, j.attempt, b.retries)
			time.AfterFunc(b.retryDelay, func() { b.queue.retry(j) })
			return
		}
		b.logf("%s: %s %v failed: %v", d.cfg.Name, j.action, j.temp, res.err)
		b.mu.Lock()
		b.applyUpdate(d, ha.State{LastCommand: fmt.Sprintf("failed: %s (%v)", describeAction(j), res.err)})
		b.mu.Unlock()
		b.publishDevice(d)
		return
	}
	metricCommands.WithLabelValues(string(j.action), "ok").Inc()
	b.logf("%s: %s %v acknowledged", d.cfg.Name, j.action, j.temp)

	b.mu.Lock()
	upd := stateFromEvent(d, res.ev)
	upd.LastCommand = "ok: " + describeAction(j)
	// Fill in what the device confirmed implicitly when its ACK carried
	// no status (or less than we asked for).
	switch j.action {
	case actionSetTemperature:
		if upd.TargetTemperature == nil {
			upd.TargetTemperature = ptr(j.temp)
		}
	case actionManual:
		if upd.ControlMode == "" {
			setControl(&upd, "manual", j.temp, 0)
			upd.TargetTemperature = ptr(j.temp)
		}
	case actionAuto:
		if upd.ControlMode == "" {
			target := ha.MaxTemp - 1 // any non-off value; refined by the next status report
			if d.state.TargetTemperature != nil {
				target = *d.state.TargetTemperature
			}
			setControl(&upd, "auto", target, 0)
		}
	case actionBoost:
		if upd.ControlMode == "" {
			target := ha.MaxTemp - 1
			if d.state.TargetTemperature != nil {
				target = *d.state.TargetTemperature
			}
			setControl(&upd, "boost", target, 5)
		}
	}
	b.applyUpdate(d, upd)
	b.mu.Unlock()
	b.publishDevice(d)
}

// applyUpdate merges upd into the device state and tracks values needed
// to translate later commands. Caller holds b.mu.
func (b *Bridge) applyUpdate(d *device, upd ha.State) {
	prevMode := d.state.ControlMode
	prevHAMode := d.state.Mode
	d.state.Merge(upd)
	b.stateDirty = true
	if t := d.state.TargetTemperature; t != nil && *t > ha.MinTemp && *t < ha.MaxTemp {
		d.lastOnTarget = *t
	}
	if d.state.ControlMode == "boost" && prevMode != "boost" {
		d.preBoostHAMode = prevHAMode // "" (unknown) leaves boost to auto
	}
	deriveState(d)
	b.updateMetrics(d)
}

func isRetryable(err error) bool {
	return errors.Is(err, uartgw.ErrNoResponse) ||
		errors.Is(err, hm.ErrNoResponse) ||
		errors.Is(err, uartgw.ErrBusy) ||
		errors.Is(err, uartgw.ErrNoCredits) ||
		errors.Is(err, uartgw.ErrTimeout) ||
		errors.Is(err, context.DeadlineExceeded)
}

// describeAction renders a climate job for the last_command sensor.
func describeAction(j job) string {
	switch j.action {
	case actionSetTemperature, actionManual:
		return fmt.Sprintf("%s %.1f", j.action, j.temp)
	}
	return string(j.action)
}
