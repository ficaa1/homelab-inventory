// Command agentboot is the installed, root-owned launcher. Upgrade it only by
// rerunning the installer; runtime releases never replace their trust anchor.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/agentboot"
	"os"
	"time"
)

var version = "dev"

func main() {
	config := flag.String("config", "", "private launcher config")
	dir := flag.String("dir", "/var/lib/homelab-agent", "state directory")
	public := flag.String("public", "/var/lib/homelab-agent/public.key", "pinned release public key")
	initialize := flag.String("initialize", "", "trusted protocol-1 runtime manifest")
	input := flag.String("input", "", "baseline artifacts")
	status := flag.Bool("status", false, "read status without the update lock")
	// Durations are local monotonic watchdog bounds, not calendar deadlines.
	timeout := flag.Duration("timeout", 6*time.Minute, "worker process group deadline")
	flag.Parse()
	c := agentboot.Config{Dir: *dir, Public: *public}
	if *config != "" {
		b, e := os.ReadFile(*config)
		if e != nil {
			fail()
		}
		if json.Unmarshal(b, &c) != nil {
			fail()
		}
	}
	host, e := os.Hostname()
	if e != nil {
		fail()
	}
	r := agentboot.Runner{Config: c, Host: host, Version: version, Timeout: *timeout}
	if *status {
		v, e := r.Load()
		if e != nil {
			fail()
		}
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			fail()
		}
		return
	}
	if *initialize != "" {
		b, e := os.ReadFile(*initialize)
		if e != nil {
			fail()
		}
		if r.Initialize(context.Background(), b, *input) != nil {
			fail()
		}
		return
	}
	if r.Run(context.Background()) != nil {
		fail()
	}
}
func fail() {
	fmt.Fprintln(os.Stderr, "agentboot: operation failed; consult safe update status")
	os.Exit(1)
}
