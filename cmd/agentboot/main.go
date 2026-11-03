// Command agentboot is the installed, root-owned launcher. Upgrade it only by
// rerunning the installer; runtime releases never replace their trust anchor.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/agentboot"
	"github.com/ficaa1/homelab-inventory/internal/agentinstall"
	"os"
	"path/filepath"
	"time"
)

var version = "dev"

func main() {
	expectedHost := flag.String("hostname", "", "expected local hostname for enrollment")
	install := flag.Bool("install", false, "explicit reversible slots installation")
	prefix := flag.String("prefix", "", "disposable systemd layout")
	scheduler := flag.String("scheduler", "systemd", "systemd or truenas-cron")
	replace := flag.String("replace-schedule", "", "explicit predecessor cron identity")
	api := flag.String("api-url", "", "API origin")
	token := flag.String("token-file", "", "private token file")
	enroll := flag.String("enroll", "", "short-lived enrollment code")
	allowHTTP := flag.Bool("allow-http", false, "explicit trusted-LAN HTTP")
	config := flag.String("config", "", "private launcher config")
	dir := flag.String("dir", "/var/lib/homelab-agent", "state directory")
	public := flag.String("public", "", "pinned release public key")
	initialize := flag.String("initialize", "", "trusted protocol-1 runtime manifest")
	input := flag.String("input", "", "baseline artifacts")
	status := flag.Bool("status", false, "read status without the update lock")
	// Durations are local monotonic watchdog bounds, not calendar deadlines.
	timeout := flag.Duration("timeout", 6*time.Minute, "worker process group deadline")
	flag.Parse()
	if *install {
		e := agentinstall.Install(context.Background(), agentinstall.Options{ExpectedHost: *expectedHost, Dir: *dir, Prefix: *prefix, URL: *api, Scheduler: *scheduler, ReplaceSchedule: *replace, TokenFile: *token, Enroll: *enroll, Public: *public, Manifest: *initialize, Input: *input, AllowHTTP: *allowHTTP})
		if e != nil {
			fmt.Fprintln(os.Stderr, "agentboot:", e)
			os.Exit(1)
		}
		return
	}
	if *config != "" {
		if handled, e := agentinstall.Resume(context.Background(), filepath.Dir(*config)); handled {
			if e != nil {
				fmt.Fprintln(os.Stderr, "agentboot:", e)
				os.Exit(1)
			}
			return
		}
	}
	c := agentboot.Config{Dir: *dir, Public: *public}
	if c.Public == "" {
		c.Public = filepath.Join(c.Dir, "public.key")
	}
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
