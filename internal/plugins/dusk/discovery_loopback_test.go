package dusk

import (
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// Explicit opt-in because the Node client lives in a separate repository. No
// multicast socket is created: both listener and client target loopback only.
func TestDiscoveryNodeLoopback(t *testing.T) {
	module := os.Getenv("DUSK_DISCOVERY_MODULE")
	if module == "" {
		t.Skip("set DUSK_DISCOVERY_MODULE to Dusk service/discovery.js")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	d := &Dusk{listen: func() (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1900})
	}}
	defer d.Close()
	check := func(want string) {
		t.Helper()
		cmd := exec.Command("node", "-e", `
const assert=require('assert'),dgram=require('dgram'),discovery=require(process.argv[1]);
let sends=0;
const client=discovery.create({socket:()=>{const s=dgram.createSocket('udp4');const bind=s.bind.bind(s),send=s.send.bind(s);s.bind=(port,cb)=>bind(port,'127.0.0.1',cb);s.setMulticastTTL=()=>{};s.send=(buf,port,host,cb)=>{assert.equal(port,1900);assert.equal(host,'239.255.255.250');sends++;send(buf,1900,'127.0.0.1',cb);};return s;}});
client.search(r=>{assert.equal(sends,1);assert.equal(r.servers.length,Number(process.argv[2]));if(r.servers.length){assert.equal(r.servers[0].baseUrl,'https://synthetic.example/inventory');assert.equal(r.servers[0].name,'Synthetic living room');assert(r.servers[0].expiresAt>Date.now());}});
`, module, want)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Node loopback: %v\n%s", err, out)
		}
	}
	cfg := plugin.Config{"lan_discovery": true, "discovery_url": "https://synthetic.example/inventory/", "server_name": "Synthetic living room"}
	if err := d.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if d.discovery != nil {
		t.Fatal("Configure opened listener")
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if d.discovery == nil {
		t.Fatal("listener not started")
	}
	check("1")
	if err := d.Configure(plugin.Config{}); err != nil {
		t.Fatal(err)
	}
	d.Start()
	if d.discovery != nil {
		t.Fatal("disabled discovery retained listener")
	}
	check("0")
	d.Configure(cfg)
	d.Start()
	d.Close()
	if d.discovery != nil {
		t.Fatal("Close retained listener")
	}
	check("0")
}
