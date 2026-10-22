// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package openccu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Group is a CCU group from /etc/config/groups.gson.
type Group struct {
	ID      string
	Name    string
	Type    string   // groupType.id, e.g. "HomeMatic.heating"
	Heating bool     // the group type is a heating group
	Members []string // device serials (channel suffix removed), in file order
}

// GroupTypeHeating is the groupType.id of BidCoS heating groups.
const GroupTypeHeating = "HomeMatic.heating"

// ParseGroups parses OpenCCU's groups.gson. The known schema is
//
//	{"groups":[{"id":1,
//	  "groupMembers":[{"memberType":{"id":"HM-CC-RT-DN"},"properties":{},"id":"NEQ0873206"}, …],
//	  "groupType":{"id":"HomeMatic.heating","label":"Heating_Control","version":3},
//	  "groupProperties":{"GROUP_DEVICE_NAME":"Room B INT0000001","NAME":"Room B", …}}, …]}
//
// If that yields no groups, a lenient walk looks for any object with a
// name and a list of members that look like device serials. lenient
// reports whether the fallback was used.
func ParseGroups(b []byte) (groups []Group, lenient bool, err error) {
	var root any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, false, err
	}
	if g := parseGroupsStrict(root); len(g) > 0 {
		return g, false, nil
	}
	return parseGroupsLenient(root), true, nil
}

func parseGroupsStrict(root any) []Group {
	top, _ := root.(map[string]any)
	list, _ := top["groups"].([]any)
	var res []Group
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		members, ok := m["groupMembers"].([]any)
		if !ok {
			continue
		}
		g := Group{ID: idString(m["id"])}
		if gt, ok := m["groupType"].(map[string]any); ok {
			g.Type, _ = gt["id"].(string)
		}
		g.Heating = strings.EqualFold(g.Type, GroupTypeHeating)
		props, _ := m["groupProperties"].(map[string]any)
		g.Name, _ = props["NAME"].(string)
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			if s, ok := props["GROUP_DEVICE_NAME"].(string); ok {
				g.Name = strings.TrimSpace(intSuffixRe.ReplaceAllString(s, ""))
			}
		}
		if g.Name == "" {
			g.Name = "Group " + g.ID
		}
		for _, me := range members {
			mm, ok := me.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := mm["id"].(string); ok && id != "" {
				g.Members = appendSerial(g.Members, id)
			}
		}
		res = append(res, g)
	}
	return res
}

var (
	intSuffixRe = regexp.MustCompile(`\s+INT\d{7}$`)
	serialRe    = regexp.MustCompile(`^[A-Za-z0-9]{10}(:\d+)?$`)
	virtualRe   = regexp.MustCompile(`^INT\d{7}`)
)

func idString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%g", t)
	}
	return fmt.Sprint(v)
}

func appendSerial(list []string, id string) []string {
	s, _, _ := strings.Cut(strings.TrimSpace(id), ":")
	s = strings.ToUpper(s)
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// parseGroupsLenient walks the JSON and treats every object with a
// name-like string and an array containing serial-like strings as a
// group.
func parseGroupsLenient(root any) []Group {
	var res []Group
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			if g, ok := lenientGroup(t); ok {
				res = append(res, g)
				return
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(root)
	return res
}

func lenientGroup(m map[string]any) (Group, bool) {
	var g Group
	var members []string
	for k, v := range m {
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		var found []string
		for _, e := range arr {
			for _, s := range stringsIn(e) {
				if serialRe.MatchString(s) && !virtualRe.MatchString(s) {
					found = appendSerial(found, s)
				}
			}
		}
		if len(found) > 0 && (len(members) == 0 || strings.Contains(strings.ToLower(k), "member")) {
			members = found
		}
	}
	if len(members) == 0 {
		return g, false
	}
	g.Members = members
	g.ID = idString(m["id"])
	g.Name = findName(m)
	if g.Name == "" {
		return g, false
	}
	for _, s := range stringsIn(withoutArrays(m)) {
		if strings.Contains(strings.ToUpper(s), "HEATING") {
			g.Heating = true
			g.Type = s
			break
		}
	}
	return g, true
}

// findName returns a name/label string of m or of a nested object
// (without descending into arrays).
func findName(m map[string]any) string {
	for _, key := range []string{"NAME", "name", "Name", "label", "title"} {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), "type") {
			continue // groupType.label is not the group's name
		}
		if sub, ok := m[k].(map[string]any); ok {
			if s := findName(sub); s != "" {
				return s
			}
		}
	}
	return ""
}

func withoutArrays(m map[string]any) map[string]any {
	res := map[string]any{}
	for k, v := range m {
		if _, ok := v.([]any); !ok {
			res[k] = v
		}
	}
	return res
}

func stringsIn(v any) []string {
	var res []string
	switch t := v.(type) {
	case string:
		res = append(res, t)
	case []any:
		for _, e := range t {
			res = append(res, stringsIn(e)...)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			res = append(res, stringsIn(t[k])...)
		}
	}
	return res
}
