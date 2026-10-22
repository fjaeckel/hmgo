// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/json"
	"strings"
	"time"
)

// Pairing: hmbridge itself appears in Home Assistant as a device
// (identifier "hmbridge_bridge") with a “Pair new device” button and
// sensors for the pairing mode, all reading the retained JSON
// PairingState on <base>/bridge/pairing.

// NameTopicSuffix is the topic suffix of a paired device's retained name.
const NameTopicSuffix = "name"

// NameTopic is the retained topic holding the name of a device paired
// with hmbridge (state of its “Name” text entity).
func (c TopicConfig) NameTopic(addr string) string {
	return c.base() + "/" + strings.ToLower(addr) + "/" + NameTopicSuffix
}

// NameMessage returns the retained name message of a paired device.
func NameMessage(cfg TopicConfig, addr, name string) Message {
	return retained(cfg.NameTopic(addr), []byte(name))
}

// PairingTopic is the retained JSON pairing state topic.
func (c TopicConfig) PairingTopic() string { return c.base() + "/bridge/pairing" }

// PairCommandTopic is the topic that starts pairing mode.
func (c TopicConfig) PairCommandTopic() string { return c.base() + "/bridge/set/pair" }

// Pairing status values.
const (
	PairingIdle        = "idle"
	PairingActive      = "active"      // waiting for a device's config button
	PairingConfiguring = "configuring" // configuring a device that announced itself
	PairingDisabled    = "disabled"    // no state_dir (nowhere to keep paired devices)
)

// PairingState is the state of pairing mode.
type PairingState struct {
	Status    string `json:"status"`    // PairingIdle, PairingActive, PairingConfiguring
	Active    bool   `json:"active"`    // pairing mode on (active or configuring)
	Remaining int    `json:"remaining"` // seconds of pairing mode left
	// LastResult describes the outcome of the last pairing or unpairing,
	// e.g. "paired HM-Sec-SCo NEQ0123456 (4c5d6e)".
	LastResult   string    `json:"last_result,omitempty"`
	LastResultAt time.Time `json:"last_result_at,omitempty"`
}

// PairingStateMessage returns the retained pairing state message.
func PairingStateMessage(cfg TopicConfig, s PairingState) Message {
	b, _ := json.Marshal(s) // only plain fields
	return retained(cfg.PairingTopic(), b)
}

const bridgeNodeID = "hmbridge_bridge"

func bridgeNode(cfg TopicConfig, swVersion string) node {
	dev := map[string]any{
		"identifiers":  []string{bridgeNodeID},
		"name":         "hmbridge",
		"model":        "hmbridge (HomeMatic BidCoS central)",
		"manufacturer": "hmbridge",
	}
	if swVersion != "" {
		dev["sw_version"] = swVersion
	}
	return node{
		id:         bridgeNodeID,
		stateTopic: cfg.PairingTopic(),
		cmdTopic: func(what string) string {
			return cfg.base() + "/bridge/set/" + what
		},
		device: dev,
	}
}

func bridgeEntities() []entity {
	return []entity{
		{component: "button", key: "pair", command: "pair", noState: true, config: map[string]any{
			"name":          "Pair new device",
			"icon":          "mdi:link-plus",
			"payload_press": "PRESS",
			"qos":           1,
		}},
		{component: "binary_sensor", key: "pairing_active", config: map[string]any{
			"name":           "Pairing mode",
			"icon":           "mdi:link-variant",
			"payload_on":     "ON",
			"payload_off":    "OFF",
			"value_template": "{{ 'ON' if value_json.active else 'OFF' }}",
		}},
		{component: "sensor", key: "pairing_remaining", config: map[string]any{
			"name":                "Pairing time remaining",
			"device_class":        "duration",
			"unit_of_measurement": "s",
			"icon":                "mdi:timer-sand",
			"value_template":      "{{ value_json.remaining if value_json.remaining is defined else 0 }}",
		}},
		{component: "sensor", key: "pairing_status", config: map[string]any{
			"name":           "Pairing status",
			"device_class":   "enum",
			"options":        []string{PairingIdle, PairingActive, PairingConfiguring, PairingDisabled},
			"icon":           "mdi:link-variant",
			"value_template": textTemplate("status", "None"),
		}},
		{component: "sensor", key: "pairing_last_result", config: map[string]any{
			"name": "Last pairing result",
			"icon": "mdi:message-text-outline",
			// HA refuses states longer than 255 characters; the full
			// text is on the status page and in the log.
			"value_template": "{{ (value_json.last_result if value_json.last_result else 'None')[:255] }}",
		}},
	}
}

// BridgeDiscoveryMessages returns the discovery messages of hmbridge's
// own HA device: the “Pair new device” button and the pairing mode
// sensors. With pairing disabled, the button is removed and the sensors
// show "disabled".
func BridgeDiscoveryMessages(cfg TopicConfig, swVersion string, pairingEnabled bool) []Message {
	n := bridgeNode(cfg, swVersion)
	var msgs []Message
	for _, e := range bridgeEntities() {
		if e.key == "pair" && !pairingEnabled {
			msgs = append(msgs, retained(entityTopic(cfg, e.component, n.id, e.key), nil))
			continue
		}
		msgs = append(msgs, entityMessage(cfg, n, e))
	}
	return msgs
}
