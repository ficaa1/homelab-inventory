// Command agentupdate posts inventory and optionally supervises signed updates.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ficaa1/homelab-inventory/internal/agentupdate"
)

func main() {
	dir := flag.String("dir", "/var/lib/homelab-agent", "private persistent state")
	pub := flag.String("public", "/var/lib/homelab-agent/public.key", "pinned public key")
	initialize := flag.String("initialize", "", "trusted signed manifest for enrollment")
	input := flag.String("input", "", "local artifact directory for enrollment")
	recover := flag.Bool("recover", false, "local rollback preserving replay state")
	selftest := flag.Bool("selftest", false, "read-only executable check")
	allowHTTP := flag.Bool("allow-http", os.Getenv("ALLOW_HTTP") == "1", "allow plain HTTP beyond loopback (credential is exposed on the network)")
	config := flag.String("config", "", "private JSON configuration")
	check := flag.Bool("check-config", false, "validate URL and token file without collecting or posting")
	configure := flag.Bool("configure", false, "write configuration; requires -config")
	signed := flag.Bool("signed", false, "enable signed updates in new configuration")
	post := flag.Bool("post-only", false, "post once without checking updates")
	discover := flag.String("discover", "/usr/local/bin/discover", "read-only collector executable")
	api := flag.String("api-url", os.Getenv("API_URL"), "API origin URL")
	tokenFile := flag.String("token-file", "", "private bearer token file; never a token argument")
	enroll := flag.String("enroll", "", "single-use enrollment code; writes agent token to -token-file")
	flag.Parse()
	if *enroll != "" {
		host, err := os.Hostname()
		if err != nil {
			fail(err)
		}
		if err := agentupdate.Enroll(context.Background(), *api, *enroll, host, *tokenFile, *allowHTTP); err != nil {
			fail(err)
		}
		return
	}
	if *selftest {
		return
	}
	c := agentupdate.Config{Dir: *dir, Public: *pub, URL: *api, TokenFile: *tokenFile, Discover: *discover, Signed: *signed, AllowHTTP: *allowHTTP}
	var err error
	if *check {
		if _, err = agentupdate.ConfigRunner(c); err != nil {
			fail(err)
		}
		if err = agentupdate.ValidateURL(c.URL, c.AllowHTTP); err != nil {
			fail(err)
		}
		return
	}
	if *configure {
		if *config == "" {
			fail(fmt.Errorf("-config required"))
		}
		if err = agentupdate.WriteConfig(*config, c); err != nil {
			fail(err)
		}
		return
	}
	if *config != "" {
		c, err = agentupdate.ReadConfig(*config, *allowHTTP)
		if err != nil {
			fail(err)
		}
	}
	host, _ := os.Hostname()
	r := agentupdate.Runner{Dir: c.Dir, Public: c.Public, URL: c.URL, Token: agentToken(), Host: host, AllowHTTP: c.AllowHTTP}
	// Enrollment and local recovery need no API credential.
	if *recover {
		err = r.Recover()
	} else if *initialize != "" {
		var b []byte
		b, err = os.ReadFile(*initialize)
		if err == nil {
			err = r.Initialize(b, *input)
		}
	} else {
		if c.TokenFile != "" {
			r, err = agentupdate.ConfigRunner(c)
			if err != nil {
				fail(err)
			}
		}
		if *post || *config != "" && !c.Signed {
			err = r.Post(context.Background(), c.Discover)
		} else {
			err = r.Run(context.Background())
		}
	}
	if err != nil {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "agentupdate: report or local operation completed")
}
func fail(err error) { fmt.Fprintln(os.Stderr, "agentupdate:", err); os.Exit(1) }

func agentToken() string {
	if t := os.Getenv("AGENT_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("API_TOKEN")
}
