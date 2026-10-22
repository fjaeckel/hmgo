// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package ha

import (
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Roles of heating group members.
const (
	RoleRadiator = "radiator" // HM-CC-RT-DN
	RoleWall     = "wall"     // HM-TC-IT-WM-W-EU
	RoleWindow   = "window"   // window contacts
)

// RoleForKind returns the group member role of a device kind ("" for
// kinds that cannot be group members).
func RoleForKind(k Kind) string {
	switch k {
	case RadiatorThermostat:
		return RoleRadiator
	case WallThermostat:
		return RoleWall
	case WindowContact:
		return RoleWindow
	}
	return ""
}

// GroupMember is a member of a heating group.
type GroupMember struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Address string `json:"address"` // 6 lowercase hex digits
	Role    string `json:"role"`    // RoleRadiator, RoleWall or RoleWindow
}

// GroupSource names the member a group value comes from.
type GroupSource struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// GroupAttributes is the retained JSON on <base>/group/<slug>/members:
// the attributes of the group's climate entity (json_attributes_topic),
// so that automations and cards can see the membership. hmbridge also
// reads it back at startup to remove the entities of former members and
// groups (StaleGroupMessages).
type GroupAttributes struct {
	Members []GroupMember `json:"members"`
	// TemperatureSource is the member providing current_temperature.
	TemperatureSource *GroupSource `json:"temperature_source,omitempty"`
	// HumiditySource is the member providing current_humidity (wall
	// thermostats only).
	HumiditySource *GroupSource `json:"humidity_source,omitempty"`
}

// GroupMembersTopic is the retained topic of a heating group's
// GroupAttributes.
func (c TopicConfig) GroupMembersTopic(slug string) string {
	return c.base() + "/group/" + slug + "/members"
}

// ParseGroupMembersTopic returns the slug of a GroupMembersTopic.
func (c TopicConfig) ParseGroupMembersTopic(topic string) (slug string, ok bool) {
	rest, found := strings.CutPrefix(topic, c.base()+"/group/")
	if !found {
		return "", false
	}
	slug, found = strings.CutSuffix(rest, "/members")
	return slug, found && ValidSlug(slug)
}

// RetainedSubscriptions returns the topic filters hmbridge reads back at
// startup (ReadRetained): StateSubscriptions plus the groups' member
// lists and settings (whose presence shows that a group had settings
// entities).
func (c TopicConfig) RetainedSubscriptions() []string {
	return append(c.StateSubscriptions(), c.base()+"/group/+/members", c.base()+"/group/+/"+SettingsTopicSuffix)
}

// Attributes returns the group's climate entity attributes.
func (g GroupDesc) Attributes() GroupAttributes {
	a := GroupAttributes{Members: g.Members}
	if a.Members == nil {
		a.Members = []GroupMember{}
	}
	for _, m := range g.Members {
		if m.Address != strings.ToLower(g.Climate) {
			continue
		}
		src := &GroupSource{Name: m.Name, Address: m.Address}
		a.TemperatureSource = src
		if m.Role == RoleWall {
			a.HumiditySource = src
		}
	}
	return a
}

// GroupMembersMessage returns the retained GroupAttributes message of g.
func GroupMembersMessage(cfg TopicConfig, g GroupDesc) Message {
	return retained(cfg.GroupMembersTopic(g.Slug), mustJSON(g.Attributes()))
}

func groupNode(cfg TopicConfig, g GroupDesc) node {
	name := g.Name
	if name == "" {
		name = g.Slug
	}
	dev := map[string]any{
		"identifiers":  []string{groupNodeID(g.Slug)},
		"name":         name + " (group)",
		"model":        "Heating group",
		"manufacturer": "hmbridge",
	}
	if g.SuggestArea && g.Name != "" {
		dev["suggested_area"] = g.Name
	}
	return node{
		id:          groupNodeID(g.Slug),
		stateTopic:  cfg.GroupStateTopic(g.Slug),
		cmdTopic:    func(what string) string { return cfg.GroupCommandTopic(g.Slug, what) },
		device:      dev,
		humidity:    true, // harmless when the group has no humidity
		deviceAvail: cfg.GroupAvailabilityTopic(g.Slug),
		attrTopic:   cfg.GroupMembersTopic(g.Slug),
		topic:       func(suffix string) string { return cfg.base() + "/group/" + g.Slug + "/" + suffix },
		haControl:   g.HAControl,
	}
}

func groupWindowEntity() entity {
	return entity{component: "binary_sensor", key: "window_open", config: binarySensor("Window", "window_open", "window", nil)}
}

func groupControlEntity() entity {
	return entity{component: "sensor", key: "control", config: controlSensorConfig()}
}

// memberEntity is an entity on a group's HA device that mirrors a
// member's state.
type memberEntity struct {
	component, key string
	config         map[string]any
	stateTopic     string
	// bridgeOnly: available while the bridge is (the connectivity
	// sensor, which shows the member's availability itself).
	bridgeOnly bool
}

// validMemberAddr reports whether addr can be used in topics.
func validMemberAddr(addr string) bool {
	_, err := hex.DecodeString(addr)
	return len(addr) == 6 && err == nil && addr == strings.ToLower(addr)
}

// memberEntities returns the entities that show member m on its group's
// device. Keys are "<addr>_<field>", so unique IDs are
// "hmbridge_group_<slug>_<addr>_<field>". The sensors have no
// state_class: the member's own sensors already record long-term
// statistics.
func memberEntities(cfg TopicConfig, m GroupMember) []memberEntity {
	addr := strings.ToLower(m.Address)
	if !validMemberAddr(addr) {
		return nil
	}
	name := m.Name
	if name == "" {
		name = m.Model + " " + strings.ToUpper(addr)
	}
	st := cfg.StateTopic(addr)
	mirror := func(c map[string]any) map[string]any {
		delete(c, "state_class")
		return c
	}
	var list []memberEntity
	switch m.Role {
	case RoleRadiator:
		list = append(list, memberEntity{component: "sensor", key: addr + "_valve", stateTopic: st,
			config: mirror(numSensor(name+" valve", "valve_position", "", "%", map[string]any{"icon": "mdi:pipe-valve"}))})
	case RoleWall:
		list = append(list,
			memberEntity{component: "sensor", key: addr + "_temperature", stateTopic: st,
				config: mirror(numSensor(name+" temperature", "current_temperature", "temperature", "°C",
					map[string]any{"suggested_display_precision": 1}))},
			memberEntity{component: "sensor", key: addr + "_humidity", stateTopic: st,
				config: mirror(numSensor(name+" humidity", "humidity", "humidity", "%", nil))})
	case RoleWindow:
		list = append(list, memberEntity{component: "binary_sensor", key: addr + "_window", stateTopic: st,
			config: binarySensor(name+" window", "window_open", "window", nil)})
	default:
		return nil
	}
	return append(list, memberEntity{component: "binary_sensor", key: addr + "_available", bridgeOnly: true,
		stateTopic: cfg.DeviceAvailabilityTopic(addr),
		config: with(diagnostic, map[string]any{
			"name":         name + " available",
			"device_class": "connectivity",
			"payload_on":   PayloadOnline,
			"payload_off":  PayloadOffline,
		})})
}

func memberMessage(cfg TopicConfig, n node, addr string, e memberEntity) Message {
	m := with(e.config)
	m["state_topic"] = e.stateTopic
	common(cfg, n, e.component, n.id+"_"+e.key, e.key, m, e.bridgeOnly)
	if !e.bridgeOnly {
		// The member's availability, not the group's.
		m["availability"] = []map[string]any{
			{"topic": cfg.AvailabilityTopic()},
			{"topic": cfg.DeviceAvailabilityTopic(addr)},
		}
		m["availability_mode"] = "all"
	}
	return retained(entityTopic(cfg, e.component, n.id, e.key), mustJSON(m))
}

// GroupDiscoveryMessages returns the discovery messages for a heating
// group, all on the group's own HA device: a climate entity (commands
// on <base>/group/<slug>/set/..., attributes from GroupMembersTopic), a
// window binary sensor, and per member entities that read the member's
// state and availability topics:
//
//	radiator thermostat  sensor "<name> valve" (%)
//	wall thermostat      sensors "<name> temperature" and "<name> humidity"
//	window contact       binary_sensor "<name> window" (device_class window)
//	every member         binary_sensor "<name> available" (connectivity, diagnostic)
//
// A group with settings (GroupDesc.Settings) also gets their config
// entities, a “Settings” and a “Settings consistency” sensor and a
// “Refresh settings” button (see GroupSettingsState).
//
// Publish GroupMembersMessage (and GroupSettingsMessage) along with them.
func GroupDiscoveryMessages(cfg TopicConfig, g GroupDesc) []Message {
	n := groupNode(cfg, g)
	msgs := []Message{
		retained(climateTopic(cfg, n.id), mustJSON(climateConfig(cfg, n))),
		entityMessage(cfg, n, groupWindowEntity()),
		entityMessage(cfg, n, groupControlEntity()),
	}
	for _, m := range g.Members {
		for _, e := range memberEntities(cfg, m) {
			msgs = append(msgs, memberMessage(cfg, n, strings.ToLower(m.Address), e))
		}
	}
	return append(msgs, groupSettingsDiscovery(cfg, n, g)...)
}

// groupMemberRemovalTopics returns the discovery topics of member m's
// entities on group slug.
func groupMemberRemovalTopics(cfg TopicConfig, slug string, m GroupMember) []string {
	var topics []string
	for _, e := range memberEntities(cfg, m) {
		topics = append(topics, entityTopic(cfg, e.component, groupNodeID(slug), e.key))
	}
	return topics
}

// GroupRemovalMessages removes a heating group's discovery configs
// (including those of the given members' entities, the settings
// entities and those of the given settings), its member list, its
// settings and its retained state.
func GroupRemovalMessages(cfg TopicConfig, slug string, members []GroupMember, settings ...SettingDesc) []Message {
	id := groupNodeID(slug)
	e, c := groupWindowEntity(), groupControlEntity()
	msgs := []Message{
		retained(climateTopic(cfg, id), nil),
		retained(entityTopic(cfg, e.component, id, e.key), nil),
		retained(entityTopic(cfg, c.component, id, c.key), nil),
	}
	for _, m := range members {
		for _, t := range groupMemberRemovalTopics(cfg, slug, m) {
			msgs = append(msgs, retained(t, nil))
		}
	}
	for _, t := range groupSettingsRemovalTopics(cfg, slug, settings) {
		msgs = append(msgs, retained(t, nil))
	}
	return append(msgs,
		retained(cfg.GroupMembersTopic(slug), nil),
		retained(cfg.GroupStateTopic(slug), nil),
		retained(cfg.GroupAvailabilityTopic(slug), nil),
	)
}

// StaleGroupMessages compares the retained messages hmbridge published
// before (read with RetainedSubscriptions) with the configured groups
// and returns removal messages for what no longer exists:
//
//   - a group that is no longer configured loses all its entities
//     (GroupRemovalMessages with the members of its last member list),
//     also one from before member lists existed (found by its state
//     topic);
//   - a configured group loses the entities of members that left it (or
//     whose role changed);
//   - a configured group that published settings before (its settings
//     topic is retained) loses the entities of settings it no longer has
//     (allSettings are all settings a group can have), and all settings
//     entities and the settings topic when it has no settings any more
//     (no thermostat with settings left).
//
// The result never touches a topic of the configured groups.
func StaleGroupMessages(cfg TopicConfig, groups []GroupDesc, retainedMsgs map[string][]byte, allSettings ...SettingDesc) []Message {
	current := map[string]bool{}
	live := map[string]bool{}
	hasSettings := map[string]bool{}
	for _, g := range groups {
		current[g.Slug] = true
		hasSettings[g.Slug] = len(g.Settings) > 0
		for _, m := range GroupDiscoveryMessages(cfg, g) {
			live[m.Topic] = true
		}
	}
	out := map[string]Message{}
	add := func(msgs ...Message) {
		for _, m := range msgs {
			if !live[m.Topic] {
				out[m.Topic] = m
			}
		}
	}
	for topic, payload := range retainedMsgs {
		if len(payload) == 0 {
			continue
		}
		if slug, ok := cfg.ParseGroupMembersTopic(topic); ok {
			var old GroupAttributes
			_ = json.Unmarshal(payload, &old) // junk: remove the group's fixed entities at least
			if !current[slug] {
				add(GroupRemovalMessages(cfg, slug, old.Members, allSettings...)...)
				continue
			}
			for _, m := range old.Members {
				for _, t := range groupMemberRemovalTopics(cfg, slug, m) {
					add(retained(t, nil))
				}
			}
			continue
		}
		if slug, ok := cfg.ParseGroupSettingsTopic(topic); ok {
			if !current[slug] {
				add(GroupRemovalMessages(cfg, slug, nil, allSettings...)...)
				continue
			}
			for _, t := range groupSettingsRemovalTopics(cfg, slug, allSettings) {
				if t == cfg.GroupSettingsTopic(slug) && hasSettings[slug] {
					continue // live state topic
				}
				add(retained(t, nil))
			}
			continue
		}
		if _, slug, ok := cfg.ParseStateTopic(topic); ok && slug != "" && ValidSlug(slug) && !current[slug] {
			add(GroupRemovalMessages(cfg, slug, nil, allSettings...)...)
		}
	}
	msgs := make([]Message, 0, len(out))
	for _, m := range out {
		msgs = append(msgs, m)
	}
	// Discovery configs first: HA should drop an entity before its state
	// topic is cleared.
	disc := func(m Message) bool { return strings.HasPrefix(m.Topic, cfg.prefix()+"/") }
	sort.Slice(msgs, func(i, j int) bool {
		if di, dj := disc(msgs[i]), disc(msgs[j]); di != dj {
			return di
		}
		return msgs[i].Topic < msgs[j].Topic
	})
	return msgs
}
