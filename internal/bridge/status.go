// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package bridge

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/fjaeckel/hmgo/internal/ha"
)

// Info is additional information shown on the status page (e.g. radio
// module firmware), set by main.
type Info map[string]string

// StatusHandler serves the status page at "/" and the state as JSON at
// "/api/state".
func (b *Bridge) StatusHandler(info Info) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		s := b.Snapshot()
		s.Extra = info
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(s)
	})
	mux.HandleFunc("/pairing/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		select {
		case b.commands <- ha.Command{Kind: ha.StartPairing}:
		case <-time.After(2 * time.Second):
			http.Error(w, "hmbridge is busy, try again", http.StatusServiceUnavailable)
			return
		case <-r.Context().Done():
			return
		}
		http.Redirect(w, r, "/#pairing", http.StatusSeeOther)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s := b.Snapshot()
		s.Extra = info
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := statusTmpl.Execute(w, s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

// sameOrigin reports whether a state-changing request comes from a page
// of this server (CSRF protection without tokens): browsers send Origin
// with every POST (or at least Referer); one of them must name this
// host. Requests without either (e.g. curl) are refused; send
// "Origin: http://<host>" to script them.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	for _, h := range []string{"Origin", "Referer"} {
		v := r.Header.Get(h)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		return err == nil && u.Host != "" && u.Host == r.Host
	}
	return false
}

var statusTmpl = template.Must(template.New("status").Funcs(template.FuncMap{
	"f1": func(v *float64) string {
		if v == nil {
			return "–"
		}
		return fmt.Sprintf("%.1f", *v)
	},
	"i": func(v *int) string {
		if v == nil {
			return "–"
		}
		return fmt.Sprint(*v)
	},
	"b": func(v *bool) string {
		if v == nil {
			return "–"
		}
		if *v {
			return "yes"
		}
		return "no"
	},
	"pct": func(v *float64) string {
		if v == nil {
			return ""
		}
		return fmt.Sprintf(" (≈%d %%)", ha.BatteryPercent(*v))
	},
	"deref": func(v *bool) bool { return v != nil && *v },
	"i64": func(v *uint64) string {
		if v == nil {
			return "0"
		}
		return fmt.Sprint(*v)
	},
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>hmbridge</title>
<style>
body{font-family:system-ui,sans-serif;margin:16px;color:#222;background:#fafafa}
h1{font-size:1.4em}h2{font-size:1.1em;margin-top:1.6em}
.wrap{overflow-x:auto}
table{border-collapse:collapse;font-size:.9em;background:#fff}
th,td{padding:4px 8px;border-bottom:1px solid #ddd;text-align:left;white-space:nowrap}
th{background:#eee}.warn{color:#b00;font-weight:600}.muted{color:#888}
@media (prefers-color-scheme:dark){body{background:#111;color:#ddd}table{background:#1a1a1a}th{background:#222}th,td{border-color:#333}}
</style></head><body>
<h1>hmbridge</h1>
<p class="muted">running since {{.Started.Format "2006-01-02 15:04:05"}}{{range $k, $v := .Extra}} · {{$k}}: {{$v}}{{end}} · command queue: {{.Queue}} · <a href="/metrics">metrics</a> · <a href="/api/state">json</a></p>

<h2 id="pairing">Pairing</h2>
{{with .Pairing}}{{if .Enabled}}
<p>Status: <b>{{.Status}}</b>{{if .Remaining}} · {{.Remaining}} s left{{end}} · AES on pairing: {{.AES}}</p>
<form method="post" action="/pairing/start"><button type="submit">Pair new device</button>
<span class="muted">then press the config button of the device within the pairing time</span></form>
{{if .LastResult}}<p>Last result ({{ago .LastResultAt}}): {{.LastResult}}</p>{{end}}
{{range .UnpairWaiting}}<p class="warn">Press the config button of {{.}} to finish its unpairing.</p>{{end}}
{{else}}<p class="muted">Pairing is disabled ({{.DisabledReason}}).</p>{{end}}
{{range .Warnings}}<p class="warn">{{.}}</p>{{end}}{{end}}

<h2>Devices</h2>
<div class="wrap"><table>
<tr><th>Name</th><th>Model</th><th>Address</th><th>Group</th><th>State</th><th>Last seen</th><th>RSSI</th><th>Battery</th><th>Low bat.</th><th>Temp.</th><th>Target</th><th>Mode</th><th>Valve</th><th>Hum.</th><th>Window</th><th>Fault</th><th>AES</th><th>Msgs</th></tr>
{{range .Devices}}<tr>
<td>{{.Name}}{{if .Paired}} <span class="muted" title="paired with hmbridge (devices file)">(paired)</span>{{end}}</td><td>{{.Model}}</td><td>{{.Address}}</td><td>{{.Group}}</td><td>{{.Summary}}</td>
<td>{{ago .State.LastSeen}}</td><td>{{i .State.RSSI}}</td>
<td>{{f1 .State.BatteryVoltage}}{{if .State.BatteryVoltage}} V{{end}}{{pct .State.BatteryVoltage}}</td>
<td{{if deref .State.LowBattery}} class="warn"{{end}}>{{b .State.LowBattery}}</td>
<td>{{f1 .State.CurrentTemperature}}</td><td>{{f1 .State.TargetTemperature}}</td>
<td>{{.State.ControlMode}}</td><td>{{i .State.ValvePosition}}</td><td>{{i .State.Humidity}}</td>
<td>{{if .State.WindowState}}{{.State.WindowState}}{{else}}{{b .State.WindowOpen}}{{end}}</td>
<td>{{.State.Fault}}</td><td>{{if .AES}}on ({{.State.AuthStatus}}){{else}}off{{end}}</td><td>{{i64 .State.MessagesReceived}}</td>
</tr>{{end}}
</table></div>

{{if .Groups}}<h2>Groups</h2>
<div class="wrap"><table>
<tr><th>Group</th><th>Members</th><th>Temp.</th><th>Target</th><th>Mode</th><th>Window open</th><th>Low battery</th><th>Fault</th></tr>
{{range .Groups}}<tr><td>{{.Name}}</td><td>{{range $i, $m := .Members}}{{if $i}}, {{end}}{{$m}}{{end}}</td>
<td>{{f1 .State.CurrentTemperature}}</td><td>{{f1 .State.TargetTemperature}}</td><td>{{.State.Mode}}</td>
<td>{{b .State.WindowOpen}}</td><td>{{b .State.LowBattery}}</td><td>{{.State.Fault}}</td></tr>{{end}}
</table></div>{{end}}

<h2>Unconfigured senders</h2>
{{if .Unknown}}<p class="muted">Radio addresses that sent messages to the central but are not in the configuration. Press a device's config button to make it send its serial number.</p>
<div class="wrap"><table>
<tr><th>Address</th><th>Messages</th><th>Last seen</th><th>Last cmd</th><th>RSSI</th><th>Serial</th><th>Model ID</th></tr>
{{range .Unknown}}<tr><td>{{.Address}}</td><td>{{.Messages}}</td><td>{{ago .LastSeen}}</td><td>{{.LastCmd}}</td><td>{{.RSSI}}</td><td>{{.Serial}}</td><td>{{.ModelID}}</td></tr>{{end}}
</table></div>{{else}}<p class="muted">none</p>{{end}}
{{if .HMIP}}
<h2>HomematicIP devices heard (not supported yet)</h2>
<p class="muted">Homematic IP radio addresses whose frames the radio module passed on. hmbridge does not decode or control Homematic IP devices and publishes nothing about them to MQTT (docs/HMIP.md).</p>
<div class="wrap"><table>
<tr><th>Address</th><th>Frames</th><th>Last seen</th><th>RSSI</th><th>Last frame</th><th>To</th></tr>
{{range .HMIP}}<tr><td>{{.Address}}</td><td>{{.Frames}}</td><td>{{ago .LastSeen}}</td><td>{{i .RSSI}}</td><td>{{.Last}}</td><td>{{.To}}</td></tr>{{end}}
</table></div>{{end}}
</body></html>`))
