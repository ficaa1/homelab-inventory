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
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()
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
	logging := flag.Bool("log", false, "bounded private scheduled output beside config")
	status := flag.Bool("status", false, "read status without the update lock")
	// Durations are local monotonic watchdog bounds, not calendar deadlines.
	timeout := flag.Duration("timeout", 6*time.Minute, "worker process group deadline")
	flag.Parse()
	if *logging && *config != "" {
		closeLog, err := scheduledLog(filepath.Dir(*config))
		if err != nil {
			return 1
		}
		defer closeLog()
	}
	if *install {
		e := agentinstall.Install(ctx, agentinstall.Options{Version: version, ExpectedHost: *expectedHost, Dir: *dir, Prefix: *prefix, URL: *api, Scheduler: *scheduler, ReplaceSchedule: *replace, TokenFile: *token, Enroll: *enroll, Public: *public, Manifest: *initialize, Input: *input, AllowHTTP: *allowHTTP})
		if e != nil {
			fmt.Fprintln(os.Stderr, "agentboot:", e)
			return 1
		}
		return 0
	}
	if *config != "" {
		if handled, e := agentinstall.Resume(ctx, filepath.Dir(*config), version); handled {
			if e != nil {
				fmt.Fprintln(os.Stderr, "agentboot:", e)
				return 1
			}
			return 0
		}
	}
	c := agentboot.Config{Dir: *dir, Public: *public}
	if c.Public == "" {
		c.Public = filepath.Join(c.Dir, "public.key")
	}
	if *config != "" {
		b, e := os.ReadFile(*config)
		if e != nil {
			return failed()
		}
		if json.Unmarshal(b, &c) != nil {
			return failed()
		}
	}
	host, e := os.Hostname()
	if e != nil {
		return failed()
	}
	r := agentboot.Runner{Config: c, Host: host, Version: version, Timeout: *timeout}
	if *status {
		v, e := r.Load()
		if e != nil {
			return failed()
		}
		if json.NewEncoder(os.Stdout).Encode(v) != nil {
			return failed()
		}
		return 0
	}
	if *initialize != "" {
		b, e := os.ReadFile(*initialize)
		if e != nil {
			return failed()
		}
		if r.Initialize(ctx, b, *input) != nil {
			return failed()
		}
		return 0
	}
	if r.Run(ctx) != nil {
		return failed()
	}
	return 0
}
func failed() int {
	fmt.Fprintln(os.Stderr, "agentboot: operation failed; consult safe update status")
	return 1
}

// Each scheduled invocation retains at most 1 MiB, plus one previous log.
func scheduledLog(dir string) (func(), error) {
	path := filepath.Join(dir, "launcher.log")
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reader.Close()
		defer f.Close()
		io.CopyN(f, reader, 1<<20)
		io.Copy(io.Discard, reader)
	}()
	return func() { os.Stdout, os.Stderr = oldOut, oldErr; writer.Close(); <-done }, nil
}
