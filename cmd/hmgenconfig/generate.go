// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/fjaeckel/hmgo/internal/config"
	"github.com/fjaeckel/hmgo/internal/hmkey"
	"github.com/fjaeckel/hmgo/internal/openccu"
)

// Sources says where the OpenCCU data comes from. An empty path or URL
// disables that source.
type Sources struct {
	RfdDir     string
	IDsPath    string
	HMModePath string
	XMLRPCURL  string
	ReGaURL    string
	GroupsPath string

	Client *http.Client
	Log    io.Writer // notes on what was used or missing
	Dump   io.Writer // raw inputs for debugging; nil disables
}

// Placeholders the user has to replace.
const (
	placeholder       = "CHANGE-ME"
	placeholderBroker = "tcp://homeassistant.local:1883"
)

// Output structures: like config.Config, but only the fields the
// generator writes, in a readable order.
type outConfig struct {
	Comment        []string     `json:"_comment,omitempty"`
	SerialPort     string       `json:"serial_port"`
	Reset          string       `json:"reset"`
	Radio          string       `json:"radio,omitempty"`
	CentralAddress string       `json:"central_address"`
	Security       *outSecurity `json:"security,omitempty"`
	MQTT           outMQTT      `json:"mqtt"`
	StateFile      string       `json:"state_file"`
	Devices        []outDevice  `json:"devices"`
	Groups         []outGroup   `json:"groups,omitempty"`
}

type outSecurity struct {
	Comment    []string `json:"_comment,omitempty"`
	Passphrase string   `json:"passphrase"`
	Index      int      `json:"index"`
	Previous   *outKey  `json:"previous,omitempty"`
}

type outKey struct {
	Comment    string `json:"_comment,omitempty"`
	Key        string `json:"key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Index      int    `json:"index"`
}

type outMQTT struct {
	Comment  string `json:"_comment,omitempty"`
	Broker   string `json:"broker"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type outDevice struct {
	Comment     any    `json:"_comment,omitempty"`
	Name        string `json:"name"`
	Model       string `json:"model"`
	Address     string `json:"address"`
	Serial      string `json:"serial,omitempty"`
	AES         bool   `json:"aes,omitempty"`
	AESChannels []int  `json:"aes_channels,omitempty"`
	KeyIndex    *int   `json:"key_index,omitempty"`
}

type outGroup struct {
	Comment string   `json:"_comment,omitempty"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// dev is one BidCoS device merged from all sources.
type dev struct {
	serial   string
	model    string
	addr     [3]byte
	hasAddr  bool
	keyIndex int // -1 unknown
	rpcAES   map[int]bool
	fileAES  map[int]bool
	name     string // from ReGa
	group    string

	outName  string
	aes      bool
	aesKnown bool
	aesChans []int
	comments []string
}

func (d *dev) addrHex() string { return strings.ToUpper(hex.EncodeToString(d.addr[:])) }

func (d *dev) supported() bool { return d.hasAddr && config.ModelChannels(d.model) > 0 }

type generator struct {
	src      Sources
	notes    []string // top-level comments
	devs     []*dev
	central  [3]byte
	hasCentr bool
}

func (g *generator) logf(format string, args ...any) {
	if g.src.Log != nil {
		fmt.Fprintf(g.src.Log, "hmgenconfig: "+format+"\n", args...)
	}
}

func (g *generator) dumpf(format string, args ...any) {
	if g.src.Dump != nil {
		fmt.Fprintf(g.src.Dump, format, args...)
	}
}

func (g *generator) bySerial(serial string) *dev {
	for _, d := range g.devs {
		if d.serial != "" && strings.EqualFold(d.serial, serial) {
			return d
		}
	}
	return nil
}

func (g *generator) byAddr(a [3]byte) *dev {
	for _, d := range g.devs {
		if d.hasAddr && d.addr == a {
			return d
		}
	}
	return nil
}

// Generate collects the OpenCCU data and returns the configuration as
// indented JSON.
func Generate(ctx context.Context, src Sources) ([]byte, error) {
	if src.Client == nil {
		src.Client = http.DefaultClient
	}
	g := &generator{src: src}
	g.readIDs()
	g.readDevFiles()
	g.readXMLRPC(ctx)
	g.readReGa(ctx)
	groups := g.readGroups()
	radio := g.readHMMode()
	return g.build(groups, radio)
}

func (g *generator) readIDs() {
	if g.src.IDsPath == "" {
		g.logf("ids file disabled")
		return
	}
	a, ok := openccu.LoadCentral(g.src.IDsPath)
	g.dumpf("== %s: BidCoS-Address found=%v %X\n", g.src.IDsPath, ok, a[:])
	if !ok {
		g.logf("no BidCoS-Address in %s", g.src.IDsPath)
		return
	}
	g.central, g.hasCentr = a, true
	g.logf("central address %X from %s", a[:], g.src.IDsPath)
}

func (g *generator) readDevFiles() {
	if g.src.RfdDir == "" {
		g.logf("rfd device files disabled")
		return
	}
	files := openccu.LoadDevices(g.src.RfdDir)
	addrs := make([][3]byte, 0, len(files))
	for a := range files {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return bytes.Compare(addrs[i][:], addrs[j][:]) < 0 })
	g.dumpf("== %s/*.dev (first lines)\n", g.src.RfdDir)
	for _, a := range addrs {
		f := files[a]
		g.dumpf("%s\n", f.FirstLine)
		if len(f.ChannelAES) > 0 {
			g.dumpf("   channel AES: %v\n", f.ChannelAES)
		}
		g.devs = append(g.devs, &dev{
			serial:   strings.ToUpper(f.Serial),
			model:    f.Type,
			addr:     f.Addr,
			hasAddr:  true,
			keyIndex: f.KeyIndexInt(),
			fileAES:  f.ChannelAES,
		})
	}
	if len(files) == 0 {
		g.logf("no device files in %s", g.src.RfdDir)
		return
	}
	g.logf("%d devices from %s/*.dev", len(files), g.src.RfdDir)
}

func (g *generator) readXMLRPC(ctx context.Context) {
	if g.src.XMLRPCURL == "" {
		g.logf("rfd XML-RPC disabled: AES settings unknown")
		return
	}
	list, err := openccu.ListDevices(ctx, g.src.Client, g.src.XMLRPCURL)
	if err != nil {
		g.logf("rfd XML-RPC listDevices at %s failed (%v): AES settings unknown; is OpenCCU running?", g.src.XMLRPCURL, err)
		g.notes = append(g.notes, "rfd's XML-RPC API was not reachable: the aes settings below are unknown (aes is off); set aes per device as OpenCCU shows it (transmission mode \"secured\").")
		return
	}
	g.dumpf("== listDevices %s (%d entries)\n", g.src.XMLRPCURL, len(list))
	nDev, nCh := 0, 0
	for _, e := range list {
		aes := "-"
		if e.AESActive != nil {
			aes = fmt.Sprint(*e.AESActive)
		}
		g.dumpf("%-16s type=%-18s parent=%-12s index=%d aes=%s rf_address=%06X\n", e.Address, e.Type, e.Parent, e.Index, aes, e.RFAddress)
		if e.IsChannel() {
			continue
		}
		nDev++
		var a [3]byte
		hasAddr := e.RFAddress > 0 && e.RFAddress <= 0xFFFFFF
		if hasAddr {
			a = [3]byte{byte(e.RFAddress >> 16), byte(e.RFAddress >> 8), byte(e.RFAddress)}
		}
		if e.Type == "HM-RCV-50" && hasAddr && !g.hasCentr {
			g.central, g.hasCentr = a, true
			g.logf("central address %X from XML-RPC (%s)", a[:], e.Address)
		}
		d := g.bySerial(e.Address)
		if d == nil && hasAddr {
			d = g.byAddr(a)
		}
		if d == nil {
			d = &dev{serial: strings.ToUpper(e.Address), keyIndex: -1}
			g.devs = append(g.devs, d)
		}
		if d.serial == "" {
			d.serial = strings.ToUpper(e.Address)
		}
		if d.model == "" {
			d.model = e.Type
		}
		if !d.hasAddr && hasAddr {
			d.addr, d.hasAddr = a, true
		}
	}
	for _, e := range list {
		if !e.IsChannel() || e.AESActive == nil || e.Index < 0 {
			continue
		}
		parent := e.Parent
		if parent == "" {
			parent = e.Serial()
		}
		d := g.bySerial(parent)
		if d == nil {
			continue
		}
		if d.rpcAES == nil {
			d.rpcAES = map[int]bool{}
		}
		d.rpcAES[e.Index] = *e.AESActive
		nCh++
	}
	g.logf("%d devices, %d channels with AES_ACTIVE from rfd XML-RPC %s", nDev, nCh, g.src.XMLRPCURL)
}

func (g *generator) readReGa(ctx context.Context) {
	if g.src.ReGaURL == "" {
		g.logf("ReGaHss disabled: devices get generated names")
		return
	}
	out, err := openccu.RunReGaScript(ctx, g.src.Client, g.src.ReGaURL, openccu.ReGaNamesScript)
	if err != nil {
		g.logf("ReGaHss script at %s failed (%v): devices get generated names; is OpenCCU running?", g.src.ReGaURL, err)
		g.notes = append(g.notes, "ReGaHss was not reachable: device names are generated; rename them as you like.")
		return
	}
	g.dumpf("== ReGa output %s\n%s\n", g.src.ReGaURL, out)
	names := openccu.ParseNames(out)
	n := 0
	for serial, name := range names {
		if d := g.bySerial(serial); d != nil {
			d.name = name
			n++
		}
	}
	g.logf("%d device names from ReGaHss %s", n, g.src.ReGaURL)
}

func (g *generator) readGroups() []openccu.Group {
	if g.src.GroupsPath == "" {
		g.logf("groups file disabled")
		return nil
	}
	b, err := os.ReadFile(g.src.GroupsPath)
	if err != nil {
		g.logf("no heating groups: %v", err)
		return nil
	}
	g.dumpf("== %s\n%s\n", g.src.GroupsPath, b)
	groups, lenient, err := openccu.ParseGroups(b)
	if err != nil {
		g.logf("cannot parse %s: %v; no groups generated", g.src.GroupsPath, err)
		return nil
	}
	if lenient {
		g.logf("%s has an unexpected format; groups were guessed (check them)", g.src.GroupsPath)
		g.notes = append(g.notes, "The groups were guessed from an unexpected groups.gson format; check them.")
	}
	heating := false
	for _, gr := range groups {
		heating = heating || gr.Heating
	}
	var res []openccu.Group
	for _, gr := range groups {
		if heating && !gr.Heating || !lenient && !gr.Heating {
			g.logf("skipping group %q of type %q (not a heating group)", gr.Name, gr.Type)
			continue
		}
		res = append(res, gr)
	}
	g.logf("%d heating groups from %s", len(res), g.src.GroupsPath)
	return res
}

func (g *generator) readHMMode() string {
	if g.src.HMModePath == "" {
		return ""
	}
	m := openccu.ReadHMMode(g.src.HMModePath)
	if m == nil {
		g.logf("%s not readable: radio is detected at startup", g.src.HMModePath)
		return ""
	}
	g.dumpf("== %s\n", g.src.HMModePath)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g.dumpf("%s=%s\n", k, m[k])
	}
	switch strings.ToUpper(m["HM_HMRF_DEV"]) {
	case "RPI-RF-MOD":
		return config.RadioRPIRFMod
	case "HM-MOD-RPI-PCB":
		return config.RadioHMModRPIPCB
	}
	g.logf("HM_HMRF_DEV=%q in %s: radio is detected at startup", m["HM_HMRF_DEV"], g.src.HMModePath)
	return ""
}

// resolveAES derives aes/aes_channels from the per-channel flags.
func (d *dev) resolveAES() {
	info, src := d.rpcAES, "rfd XML-RPC"
	if len(info) == 0 {
		info, src = d.fileAES, "the .dev file"
	}
	if len(info) == 0 {
		return
	}
	d.aesKnown = true
	n := config.ModelChannels(d.model)
	var on []int
	off := 0
	for c := 1; c <= n; c++ {
		v, ok := info[c]
		switch {
		case ok && v:
			on = append(on, c)
		case ok:
			off++
		}
	}
	if len(on) == 0 {
		return
	}
	d.aes = true
	if off > 0 {
		d.aesChans = on
	}
	if src != "rfd XML-RPC" {
		d.comments = append(d.comments, "AES taken from "+src)
	}
}

// mostCommon returns the most frequent value (ties: the larger one), or
// -1 for an empty map.
func mostCommon(count map[int]int) int {
	best, bestN := -1, 0
	for v, n := range count {
		if n > bestN || n == bestN && v > best {
			best, bestN = v, n
		}
	}
	return best
}

func (g *generator) build(groups []openccu.Group, radio string) ([]byte, error) {
	cfg := outConfig{
		SerialPort: "/dev/raw-uart",
		Reset:      "none",
		Radio:      radio,
		MQTT: outMQTT{
			Comment:  "Fill in your MQTT broker (e.g. the Mosquitto add-on of Home Assistant) and its credentials.",
			Broker:   placeholderBroker,
			Username: placeholder,
			Password: placeholder,
		},
		Devices: []outDevice{},
	}
	cfg.Comment = append(cfg.Comment,
		"Generated by hmgenconfig from OpenCCU's device list. Review it before the first run.",
		"Fill in security.passphrase (OpenCCU's system security key, never read by hmgenconfig) and the mqtt settings, then check: hmbridge -config config.json -check_config",
		"state_file is \"\" because hmbridge runs from OpenCCU here; the state is restored from the broker's retained messages. Set a writable path (e.g. /usr/local/hmbridge/state.json) if you want the fallback file.",
	)
	if g.hasCentr {
		cfg.CentralAddress = strings.ToUpper(hex.EncodeToString(g.central[:]))
	} else {
		cfg.CentralAddress = placeholder
		g.logf("central address unknown: fill in central_address (BidCoS-Address in /etc/config/ids, as hex)")
		cfg.Comment = append(cfg.Comment, "central_address could not be read: set it to OpenCCU's BidCoS address in hex (BidCoS-Address in /etc/config/ids is decimal).")
	}

	var supported []*dev
	var unsupported []string
	for _, d := range g.devs {
		if g.hasCentr && d.hasAddr && d.addr == g.central && d.model != "HM-RCV-50" {
			g.logf("device %s has the central's address; skipped", d.serial)
			continue
		}
		if !d.supported() {
			name := d.name
			if name == "" {
				name = d.serial
			}
			where := d.serial
			if d.hasAddr {
				where = d.addrHex()
			}
			unsupported = append(unsupported, fmt.Sprintf("not supported yet: %s (%s %s)", name, d.model, where))
			continue
		}
		d.resolveAES()
		supported = append(supported, d)
	}
	sort.Strings(unsupported)

	// Groups: map member serials to supported devices.
	groupNames := map[string]bool{}
	var outGroups []outGroup
	groupMembers := map[string][]*dev{}
	for _, gr := range groups {
		name := gr.Name
		for i := 2; groupNames[strings.ToLower(name)]; i++ {
			name = fmt.Sprintf("%s %d", gr.Name, i)
		}
		var members []*dev
		thermostats := 0
		for _, s := range gr.Members {
			var d *dev
			for _, sd := range supported {
				if strings.EqualFold(sd.serial, s) {
					d = sd
				}
			}
			if d == nil {
				g.logf("group %q: member %s is not a supported device; left out", gr.Name, s)
				continue
			}
			if d.group != "" {
				g.logf("group %q: %s is already in group %q; left out", gr.Name, s, d.group)
				continue
			}
			members = append(members, d)
			if config.IsThermostat(d.model) {
				thermostats++
			}
		}
		if thermostats == 0 {
			g.logf("group %q has no supported thermostat; skipped", gr.Name)
			continue
		}
		groupNames[strings.ToLower(name)] = true
		for _, d := range members {
			d.group = name
		}
		groupMembers[name] = members
		outGroups = append(outGroups, outGroup{Name: name, Comment: "OpenCCU heating group " + gr.Name})
	}

	// Names: ReGa name, else derived; names used more than once get the
	// serial appended.
	for _, d := range supported {
		switch {
		case d.name != "":
			d.outName = d.name
		case d.group != "":
			d.outName = d.group + " " + d.model
		case d.serial != "":
			d.outName = d.model + " " + d.serial
		default:
			d.outName = d.model + " " + d.addrHex()
		}
	}
	count := map[string]int{}
	for _, d := range supported {
		count[strings.ToLower(d.outName)]++
	}
	for _, d := range supported {
		if count[strings.ToLower(d.outName)] > 1 {
			suffix := d.serial
			if suffix == "" {
				suffix = d.addrHex()
			}
			d.outName += " " + suffix
		}
	}
	seen := map[string]bool{}
	for _, d := range supported {
		for seen[strings.ToLower(d.outName)] {
			d.outName += " " + d.addrHex()
		}
		seen[strings.ToLower(d.outName)] = true
	}

	sort.SliceStable(supported, func(i, j int) bool {
		a, b := supported[i], supported[j]
		if (a.group == "") != (b.group == "") {
			return a.group != ""
		}
		if a.group != b.group {
			return strings.ToLower(a.group) < strings.ToLower(b.group)
		}
		return strings.ToLower(a.outName) < strings.ToLower(b.outName)
	})
	sort.SliceStable(outGroups, func(i, j int) bool {
		return strings.ToLower(outGroups[i].Name) < strings.ToLower(outGroups[j].Name)
	})
	for i := range outGroups {
		for _, d := range supported {
			if d.group == outGroups[i].Name {
				outGroups[i].Members = append(outGroups[i].Members, d.outName)
			}
		}
	}

	// Key indexes: the most common index of the AES devices is the
	// current key, the next most common the previous one.
	idx := map[int]int{}
	for _, d := range supported {
		if d.keyIndex >= 0 && (d.aes || !d.aesKnown) {
			idx[d.keyIndex]++
		}
	}
	if len(idx) == 0 {
		for _, d := range supported {
			if d.keyIndex >= 0 {
				idx[d.keyIndex]++
			}
		}
	}
	current := mostCommon(idx)
	prevIdx := -1
	if current > 0 {
		cfg.Security = &outSecurity{
			Comment: []string{
				fmt.Sprintf("OpenCCU's devices use key index %d. Replace CHANGE-ME with OpenCCU's system security key (passphrase).", current),
			},
			Passphrase: placeholder,
			Index:      current,
		}
		delete(idx, current)
		prevIdx = mostCommon(idx)
		if prevIdx >= 0 {
			pk := &outKey{Index: prevIdx}
			if prevIdx == 0 {
				pk.Comment = "The eQ-3 default key (index 0), used by devices that never received the system security key."
				pk.Key = strings.ToUpper(hex.EncodeToString(hmkey.EQ3Default.Bytes[:]))
			} else {
				pk.Comment = fmt.Sprintf("Some devices still use key index %d: replace CHANGE-ME-PREVIOUS with that older passphrase, or re-key the devices in OpenCCU and remove this block and their key_index.", prevIdx)
				pk.Passphrase = placeholder + "-PREVIOUS"
			}
			cfg.Security.Previous = pk
		}
	} else if current == 0 {
		cfg.Comment = append(cfg.Comment, "All devices use the eQ-3 default key (index 0): no security block needed.")
	}

	for _, d := range supported {
		od := outDevice{
			Name:        d.outName,
			Model:       d.model,
			Address:     d.addrHex(),
			AES:         d.aes,
			AESChannels: d.aesChans,
		}
		if len(d.serial) == 10 {
			od.Serial = d.serial
		}
		if d.name == "" {
			d.comments = append(d.comments, "name generated (not found in ReGaHss)")
		}
		if !d.aesKnown {
			d.comments = append(d.comments, "AES state unknown: set \"aes\": true if OpenCCU shows the device's channels as secured")
		}
		if cfg.Security != nil && d.keyIndex >= 0 && d.keyIndex != current && (d.aes || !d.aesKnown) {
			if d.keyIndex == prevIdx {
				ki := d.keyIndex
				od.KeyIndex = &ki
			} else {
				d.comments = append(d.comments, fmt.Sprintf("uses key index %d, which is neither the current nor the previous key: re-key it in OpenCCU", d.keyIndex))
			}
		}
		switch len(d.comments) {
		case 0:
		case 1:
			od.Comment = d.comments[0]
		default:
			od.Comment = d.comments
		}
		cfg.Devices = append(cfg.Devices, od)
	}
	cfg.Groups = outGroups
	cfg.Comment = append(cfg.Comment, unsupported...)
	if len(cfg.Devices) == 0 {
		g.logf("no supported devices found; the configuration needs at least one device")
	}
	g.logf("%d devices, %d groups written; %d devices not supported", len(cfg.Devices), len(cfg.Groups), len(unsupported))

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
