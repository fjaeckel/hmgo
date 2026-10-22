// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package openccu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XMLRPCCall performs a minimal XML-RPC call with string parameters and
// returns the decoded result: int64, bool, float64, string, []byte,
// []any or map[string]any.
func XMLRPCCall(ctx context.Context, client *http.Client, url, method string, params ...string) (any, error) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0"?><methodCall><methodName>`)
	xml.EscapeText(&body, []byte(method))
	body.WriteString(`</methodName><params>`)
	for _, p := range params {
		body.WriteString(`<param><value><string>`)
		xml.EscapeText(&body, []byte(p))
		body.WriteString(`</string></value></param>`)
	}
	body.WriteString(`</params></methodCall>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %s", url, resp.Status)
	}
	return ParseXMLRPCResponse(b)
}

// ParseXMLRPCResponse decodes a methodResponse document. A fault is
// returned as an error.
func ParseXMLRPCResponse(b []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.CharsetReader = charsetReader
	fault := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, errors.New("XML-RPC: no value in response")
		}
		if err != nil {
			return nil, fmt.Errorf("XML-RPC: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "fault":
			fault = true
		case "value":
			v, err := parseValue(d)
			if err != nil {
				return nil, fmt.Errorf("XML-RPC: %w", err)
			}
			if fault {
				if m, ok := v.(map[string]any); ok {
					return nil, fmt.Errorf("XML-RPC fault %v: %v", m["faultCode"], m["faultString"])
				}
				return nil, fmt.Errorf("XML-RPC fault: %v", v)
			}
			return v, nil
		}
	}
}

// parseValue decodes the content of a <value> element whose start tag
// was just read, consuming its end tag.
func parseValue(d *xml.Decoder) (any, error) {
	var text strings.Builder
	var res any
	typed := false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if !typed {
				return text.String(), nil // untyped value = string
			}
			return res, nil
		case xml.StartElement:
			typed = true
			if res, err = parseTyped(d, t); err != nil {
				return nil, err
			}
		}
	}
}

func parseTyped(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "array":
		var arr []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "value" {
					v, err := parseValue(d)
					if err != nil {
						return nil, err
					}
					arr = append(arr, v)
				} // <data> is entered implicitly
			case xml.EndElement:
				if t.Name.Local == "array" {
					if arr == nil {
						arr = []any{}
					}
					return arr, nil
				}
			}
		}
	case "struct":
		m := map[string]any{}
		var name string
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "name":
					if name, err = readText(d); err != nil {
						return nil, err
					}
				case "value":
					v, err := parseValue(d)
					if err != nil {
						return nil, err
					}
					m[strings.TrimSpace(name)] = v
				}
			case xml.EndElement:
				if t.Name.Local == "struct" {
					return m, nil
				}
			}
		}
	case "nil":
		return nil, d.Skip()
	}
	s, err := readText(d)
	if err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "i4", "int", "i8", "i2", "i1":
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	case "boolean":
		b, ok := parseBool(s)
		if !ok {
			return nil, fmt.Errorf("invalid boolean %q", s)
		}
		return b, nil
	case "double":
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	case "base64":
		return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	}
	return s, nil // string, dateTime.iso8601, unknown
}

// readText returns the character data up to the end of the current
// element (nested elements are skipped).
func readText(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return sb.String(), nil
			}
			depth--
		}
	}
}

func charsetReader(charset string, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.ReplaceAll(charset, "_", "-")) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1", "iso-8859-15", "windows-1252", "cp1252", "us-ascii", "ascii":
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(Latin1ToUTF8(b)), nil
	case "utf-8", "utf8":
		return r, nil
	}
	return nil, fmt.Errorf("unsupported charset %q", charset)
}

// Latin1ToUTF8 converts ISO-8859-1 bytes to a UTF-8 string.
func Latin1ToUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// DecodeText returns b as UTF-8: unchanged if it already is valid
// UTF-8, else converted from ISO-8859-1 (ReGaHss's encoding).
func DecodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return Latin1ToUTF8(b)
}

// DeviceDescription is one entry of rfd's listDevices: a device
// (Parent == "") or one of its channels (Address "SERIAL:N").
type DeviceDescription struct {
	Address   string // serial, or serial:channel
	Type      string
	Parent    string
	Index     int // channel number (-1 for devices)
	AESActive *bool
	RFAddress int64 // devices only, 0 if absent
}

// Serial returns the device serial of the entry (the part before ':').
func (d DeviceDescription) Serial() string {
	s, _, _ := strings.Cut(d.Address, ":")
	return s
}

// IsChannel reports whether the entry describes a channel.
func (d DeviceDescription) IsChannel() bool { return strings.Contains(d.Address, ":") }

// ListDevices calls listDevices on rfd's XML-RPC API (e.g.
// http://127.0.0.1:2001/).
func ListDevices(ctx context.Context, client *http.Client, url string) ([]DeviceDescription, error) {
	v, err := XMLRPCCall(ctx, client, url, "listDevices")
	if err != nil {
		return nil, err
	}
	return DecodeDeviceList(v)
}

// DecodeDeviceList converts a decoded listDevices result.
func DecodeDeviceList(v any) ([]DeviceDescription, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("listDevices: result is %T, want array", v)
	}
	var res []DeviceDescription
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		dd := DeviceDescription{
			Address: strings.TrimSpace(asString(m["ADDRESS"])),
			Type:    strings.TrimSpace(asString(m["TYPE"])),
			Parent:  strings.TrimSpace(asString(m["PARENT"])),
			Index:   -1,
		}
		if dd.Address == "" {
			continue
		}
		if n, ok := asInt(m["INDEX"]); ok {
			dd.Index = int(n)
		} else if _, ch, ok := strings.Cut(dd.Address, ":"); ok {
			if n, err := strconv.Atoi(ch); err == nil {
				dd.Index = n
			}
		}
		if b, ok := asBool(m["AES_ACTIVE"]); ok {
			dd.AESActive = &b
		}
		if n, ok := asInt(m["RF_ADDRESS"]); ok {
			dd.RFAddress = n
		}
		res = append(res, dd)
	}
	return res, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func asInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 0, 64)
		return n, err == nil
	}
	return 0, false
}

func asBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case int64:
		return t != 0, true
	case float64:
		return t != 0, true
	case string:
		return parseBool(t)
	}
	return false, false
}
