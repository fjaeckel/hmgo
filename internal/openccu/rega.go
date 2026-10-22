// Copyright (c) 2026 Frederic Jung
// SPDX-License-Identifier: BSD-3-Clause

package openccu

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ReGaNamesScript lists the BidCos-RF devices known to ReGaHss as
// "<serial>\t<name>" lines.
const ReGaNamesScript = `string sId;
object oDev;
object oIf;
string sIf;
foreach (sId, dom.GetObject(ID_DEVICES).EnumUsedIDs()) {
  oDev = dom.GetObject(sId);
  if (oDev) {
    sIf = "";
    oIf = dom.GetObject(oDev.Interface());
    if (oIf) { sIf = oIf.Name(); }
    if ((sIf == "BidCos-RF") || ((sIf == "") && (oDev.Address().Find(":") == -1))) {
      WriteLine(oDev.Address() # "\t" # oDev.Name());
    }
  }
}
`

// RunReGaScript posts script to ReGaHss (e.g.
// http://127.0.0.1:8183/rega.exe) and returns its output as UTF-8, with
// the XML trailer (<xml><exec>…</exec>…</xml>) removed.
func RunReGaScript(ctx context.Context, client *http.Client, url, script string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(script))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %s", url, resp.Status)
	}
	return StripReGaTrailer(DecodeText(b)), nil
}

// StripReGaTrailer removes the <xml>…</xml> block ReGaHss appends to
// the script output.
func StripReGaTrailer(s string) string {
	if i := strings.LastIndex(s, "<xml><exec>"); i >= 0 {
		return s[:i]
	}
	if i := strings.LastIndex(s, "<xml>"); i >= 0 && strings.HasSuffix(strings.TrimSpace(s), "</xml>") {
		return s[:i]
	}
	return s
}

// ParseNames parses "<serial>\t<name>" lines into a map by upper-case
// serial. Lines without a tab and empty names are ignored.
func ParseNames(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		serial, name, ok := strings.Cut(line, "\t")
		serial, name = strings.TrimSpace(serial), strings.TrimSpace(name)
		if !ok || serial == "" || name == "" {
			continue
		}
		res[strings.ToUpper(serial)] = name
	}
	return res
}
