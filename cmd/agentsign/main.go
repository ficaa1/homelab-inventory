// Command agentsign runs on the signer machine, outside the API container.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func main() {
	platforms := flag.Bool("platforms", false, "print configured build platforms")
	key := flag.String("key", "release-ed25519.key", "signer-owned mode-0600 private key")
	dir := flag.String("dir", "releases", "release directory")
	input := flag.String("input", "", "staged artifacts")
	version := flag.String("version", "", "build version")
	resign := flag.Bool("resign", false, "re-sign held current manifest for an explicit key rotation")
	keygen := flag.Bool("keygen", false, "create key if absent, print only public key")
	control := flag.String("control", "", "hold, promote or auto current release")
	flag.Parse()
	if *platforms {
		ps, err := agentrelease.Platforms(os.Getenv("AGENT_PLATFORMS"))
		if err != nil {
			fail(err)
		}
		for _, p := range ps {
			fmt.Println(p)
		}
		return
	}
	if *keygen {
		pub, e := agentrelease.Keygen(*key)
		if e != nil {
			fail(e)
		}
		fmt.Print(string(pub))
		return
	}
	if e := os.MkdirAll(*dir, 0755); e != nil {
		fail(e)
	}
	lock, e := os.OpenFile(filepath.Join(*dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		fail(e)
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		fail(e)
	}
	if *resign {
		if e = agentrelease.Resign(*dir, *key); e != nil {
			fail(e)
		}
		return
	}
	if *control != "" {
		if *control != "hold" && *control != "promote" && *control != "auto" {
			fail(fmt.Errorf("invalid control"))
		}
		m, _, _, e := agentrelease.Read(*dir)
		if e != nil {
			fail(e)
		}
		e = agentrelease.WriteJSON(filepath.Join(*dir, "control.json"), agentrelease.Control{Sequence: m.Sequence, Mode: *control})
		if e != nil {
			fail(e)
		}
		return
	}
	if *input == "" || *version == "" {
		fail(fmt.Errorf("input and version required"))
	}
	if e = agentrelease.Publish(*dir, *key, *input, *version, inventory.AgentRevision); e != nil {
		fail(e)
	}
}
func fail(e error) { fmt.Fprintln(os.Stderr, "agentsign:", e); os.Exit(1) }
