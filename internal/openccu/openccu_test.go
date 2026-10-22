// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package openccu

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseXMLRPC(t *testing.T) {
	v, err := ParseXMLRPCResponse([]byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<methodResponse><params><param><value><array><data>" +
		"<value><struct><member><name>ADDRESS</name><value>K\xfcche</value></member>" +
		"<member><name>N</name><value><i4>-3</i4></value></member>" +
		"<member><name>B</name><value><boolean>1</boolean></value></member>" +
		"<member><name>D</name><value><double>1.5</double></value></member>" +
		"<member><name>E</name><value><array><data></data></array></value></member>" +
		"</struct></value></data></array></value></param></params></methodResponse>"))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"ADDRESS": "Küche", "N": int64(-3), "B": true, "D": 1.5, "E": []any{}}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("got %#v", v)
	}

	_, err = ParseXMLRPCResponse([]byte(`<methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-1</i4></value></member><member><name>faultString</name><value>Failure</value></member></struct></value></fault></methodResponse>`))
	if err == nil || !strings.Contains(err.Error(), "Failure") {
		t.Errorf("fault: %v", err)
	}
}

func TestReGa(t *testing.T) {
	out := StripReGaTrailer(DecodeText([]byte("NEQ1\tB\xfcro\nbad line\nX:1\t\n<xml><exec>/rega.exe</exec><sId>1</sId></xml>")))
	if got := ParseNames(out); !reflect.DeepEqual(got, map[string]string{"NEQ1": "Büro"}) {
		t.Errorf("names = %v", got)
	}
	if got := DecodeText([]byte("Büro")); got != "Büro" {
		t.Errorf("UTF-8 input changed: %q", got)
	}
}

func TestDeviceFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.dev"), []byte(`<device serial="NEQ0882326" type="HM-CC-RT-DN" address="0x4DA5A9" aes_key_index="2">
<channels>
 <channel index="1" aes_active="true"/>
 <channel index="2" aes_active="0" aes_key_index="2"/>
 <channel type="x"/>
</channels>
</device>`), 0o644)
	devs := LoadDevices(dir)
	d, ok := devs[[3]byte{0x4D, 0xA5, 0xA9}]
	if !ok || d.KeyIndexInt() != 2 || d.Serial != "NEQ0882326" {
		t.Fatalf("devices = %+v", devs)
	}
	if !reflect.DeepEqual(d.ChannelAES, map[int]bool{1: true, 2: false}) {
		t.Errorf("channel AES = %v", d.ChannelAES)
	}
}

func TestGroupsLenient(t *testing.T) {
	g, lenient, err := ParseGroups([]byte(`{"list":[{"label":"Bad","type":"HEATING","members":["NEQ0000001:1",{"address":"MEQ0000002"}]},{"label":"Other","members":["NEQ0000003"]}]}`))
	if err != nil || !lenient || len(g) != 2 {
		t.Fatalf("groups = %+v lenient %v err %v", g, lenient, err)
	}
	if g[0].Name != "Bad" || !g[0].Heating || !reflect.DeepEqual(g[0].Members, []string{"NEQ0000001", "MEQ0000002"}) || g[1].Heating {
		t.Errorf("groups = %+v", g)
	}
}

func TestGroupsStrictNameFallback(t *testing.T) {
	g, lenient, err := ParseGroups([]byte(`{"groups":[{"id":7,"groupMembers":[{"id":"NEQ0000001"}],"groupType":{"id":"HomeMatic.heating"},"groupProperties":{"GROUP_DEVICE_NAME":"Bad oben INT0000007"}}]}`))
	if err != nil || lenient || len(g) != 1 || g[0].Name != "Bad oben" || !g[0].Heating {
		t.Errorf("groups = %+v lenient %v err %v", g, lenient, err)
	}
}

func TestHMModeAndCentral(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hm_mode")
	os.WriteFile(p, []byte("HM_HMRF_DEV='RPI-RF-MOD'\n"), 0o644)
	if m := ReadHMMode(p); m["HM_HMRF_DEV"] != "RPI-RF-MOD" {
		t.Errorf("hm_mode = %v", m)
	}
	ids := filepath.Join(dir, "ids")
	os.WriteFile(ids, []byte("BidCoS-Address = 4554886\n"), 0o644)
	if a, ok := LoadCentral(ids); !ok || a != [3]byte{0x45, 0x80, 0x86} {
		t.Errorf("central = %X %v", a, ok)
	}
}
