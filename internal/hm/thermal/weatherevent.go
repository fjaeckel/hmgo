package thermal

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	weatherEventTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "WeatherEventTemperature",
			Help:      "Temperature in degC",
		},
		[]string{"address", "name"})

	weatherEventHumidity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: prometheusNamespace,
			Name:      "WeatherEventHumidity",
			Help:      "Humidity in percentage points",
		},
		[]string{"address", "name"})
)

func init() {
	prometheus.MustRegister(weatherEventTemperature)
	prometheus.MustRegister(weatherEventHumidity)
}

type WeatherEvent struct {
	Temperature float64 // in degC
	Humidity    uint64  // in percentage points
}

var weTmpl = template.Must(template.New("weatherevent").Parse(`
<strong>Weather:</strong><br>
Temperature: {{ .Temperature }} ℃<br>
Humidity: {{ .Humidity }}%<br>
`))

func (we *WeatherEvent) HTML() template.HTML {
	var buf bytes.Buffer
	if err := weTmpl.Execute(&buf, we); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

func (tc *ThermalControl) DecodeWeatherEvent(payload []byte) (*WeatherEvent, error) {
	// c.f. <frame id="WEATHER_EVENT"> in rftypes/tc.xml: TEMPERATURE is
	// a 15 bit signed value (×0.1 °C) in [0]&0x7f,[1]; HUMIDITY = [2] (%).
	if got, want := len(payload), 3; got != want {
		return nil, fmt.Errorf("unexpected payload size: got %d, want %d", got, want)
	}
	t := int(payload[0]&0x7f)<<8 | int(payload[1])
	if t&0x4000 != 0 {
		t -= 0x8000
	}
	we := &WeatherEvent{
		Temperature: float64(t) / 10,
		Humidity:    uint64(payload[2]),
	}

	weatherEventTemperature.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(we.Temperature)
	weatherEventHumidity.With(prometheus.Labels{"name": tc.Name(), "address": tc.AddrHex()}).Set(float64(we.Humidity))

	tc.latestMu.Lock()
	defer tc.latestMu.Unlock()
	tc.latestWeatherEvent = we
	return we, nil
}
