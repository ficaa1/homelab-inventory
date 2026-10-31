package dusk

import (
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"strings"
	"testing"
)

func TestDiscoveryContract(t *testing.T) {
	r, e := discoveryResponse("https://home.example/prefix/", "Living room")
	if e != nil || !strings.Contains(r, "LOCATION: https://home.example/prefix\r\n") {
		t.Fatal(r, e)
	}
	for _, u := range []string{"", "http://user:secret@host", "http://host/?token=a", "http://host/#a", "file:///tmp/x", "http://host/\r\nInjected: x"} {
		if _, e := discoveryResponse(u, "Home"); e == nil {
			t.Errorf("accepted %q", u)
		}
	}
	q := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: " + discoveryType + "\r\n\r\n"
	if !isSearch([]byte(q)) {
		t.Fatal("valid search refused")
	}
	for _, bad := range []string{strings.Replace(q, "MX: 1", "MX: 9", 1), strings.Replace(q, "ST: ", "ST: other\r\nST: ", 1), strings.Repeat("x", 2049)} {
		if isSearch([]byte(bad)) {
			t.Fatal("accepted invalid search")
		}
	}
	d := &Dusk{}
	if e := d.Configure(plugin.Config{"lan_discovery": true, "discovery_url": "file:///tmp/x"}); e == nil {
		t.Fatal("bad URL")
	}
	if e := d.Configure(plugin.Config{}); e != nil {
		t.Fatal(e)
	}
	d.Close()
	d.Close()
}
