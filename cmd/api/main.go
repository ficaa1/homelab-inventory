// Command api is the ingest and query service: it accepts the same
// JSON discover emits, keeps a history of it per host, and serves derived
// views (currently the port map) over HTTP.
//
// It is deliberately thin. Storage lives in internal/store, cross-host
// aggregation in internal/portmap, threshold rules in internal/alert and
// notification bookkeeping in internal/notify; this package only decodes HTTP
// requests, calls into those packages, and encodes the response.
//
//	api -addr 127.0.0.1:8080 -allow-unauthenticated # local development
//	api -addr :9000 -dir /var/lib/homelab-inventory
//	API_TOKEN=... api            # authenticated inventory service
//	APPRISE_URL=... api          # notify on alert transitions
//	api -notify-level warning    # notify about warnings too, not just criticals
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/icons"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/settings"
	"github.com/ficaa1/homelab-inventory/internal/setup"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}

// envDuration falls back to dflt on anything unparseable rather than failing
// to start: a typo in an optional notification setting should not take the
// ingest endpoint down with it. The chosen value is logged either way.
func envDuration(key string, dflt time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return dflt
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "api: ignoring %s=%q (%v); using %s\n", key, v, err, dflt)
		return dflt
	}
	return d
}

func run() error {
	started := time.Now()
	var (
		addr = flag.String("addr", ":8080", "address to listen on")
		dir  = flag.String("dir", "data", "directory to store inventories in")
		// Notifications fire on transitions, so this interval is how quickly a
		// change is noticed, not how often anything is sent. Five minutes is
		// well inside the agents' 15-minute push, so nothing is missed, and
		// three quiet evaluations per push cost nothing.
		//
		// Both notification flags default from the environment so the
		// container's behaviour can be changed by editing deploy/.env and
		// restarting, without rebuilding an image or editing a compose
		// command. API_TOKEN already arrives that way.
		notifyEvery = flag.Duration("notify-interval", envDuration("NOTIFY_INTERVAL", 5*time.Minute),
			"how often to evaluate alerts for notification")
		notifyLevel = flag.String("notify-level", envOr("NOTIFY_LEVEL", "critical"),
			"minimum severity to notify about: critical, warning or info")
		// The digest is a different job from the transition notifications: it
		// answers "what is the state of things" rather than "something just
		// happened", and it is the only thing that mentions the slow problems
		// the transition notifier is designed never to repeat. 0 disables it.
		digestEvery = flag.Duration("digest-interval", envDuration("DIGEST_INTERVAL", 168*time.Hour),
			"how often to send a summary digest; 0 disables it")
	)
	allowOpen := flag.Bool("allow-unauthenticated", false, "development only: allow an unset API_TOKEN")
	flag.Parse()

	if *allowOpen && !loopbackAddress(*addr) {
		return errors.New("-allow-unauthenticated requires a loopback listen address (e.g. 127.0.0.1:8080)")
	}

	st, err := store.Open(*dir)
	if err != nil {
		return err
	}

	manager, err := setup.Open(*dir, setup.Tokens{API: os.Getenv("API_TOKEN"), Agent: os.Getenv("AGENT_TOKEN"), Operator: os.Getenv("OPERATOR_TOKEN")}, started, setup.Window, time.Now)
	if err != nil {
		return err
	}
	tokens := manager.Tokens()
	if err := validateTokens(tokens.API, tokens.Agent, tokens.Operator); err != nil {
		return err
	}
	if _, err := setupOrigin(os.Getenv("PUBLIC_URL")); err != nil && os.Getenv("PUBLIC_URL") != "" {
		return fmt.Errorf("invalid PUBLIC_URL: %w", err)
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	srv := &server{store: st, log: logger, token: tokens.API, agentToken: tokens.Agent, operatorToken: tokens.Operator, setup: manager, agentBinDir: envOr("AGENT_BIN_DIR", "/usr/share/homelab-inventory/agent"), publicURL: os.Getenv("PUBLIC_URL"), trustedProxy: os.Getenv("TRUSTED_PROXY") == "1", actionPolicy: execallow.Policy{Host: os.Getenv("ACTIONS_HOST")}}

	// Launcher icons. Fetching is off unless asked for: retrieving one tells a
	// third-party repository which services run here, which is small but real,
	// and the dashboard is perfectly usable with lettered tiles instead.
	fetchIcons := os.Getenv("ICONS_FETCH") == "1"
	srv.icons = icons.NewCache(st, fetchIcons)
	srv.icons.Log = logger.Printf
	if fetchIcons {
		logger.Println("icon fetching on: icons are downloaded once, cached, and served from here")
	} else {
		logger.Println("ICONS_FETCH is not 1; launcher tiles use lettered placeholders")
	}
	if *allowOpen {
		srv.setup = nil
	}
	if srv.token == "" && *allowOpen {
		logger.Println("development mode: API_TOKEN is unset; ordinary routes have no bearer authentication")
	}

	if srv.token == "" && !*allowOpen {
		setupAddr := *addr
		if host, port, err := net.SplitHostPort(setupAddr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			setupAddr = net.JoinHostPort("localhost", port)
		}
		logger.Printf("setup mode: open http://%s/ within 10 minutes to claim this instance", setupAddr)
	}
	if srv.token != "" && srv.agentToken == "" {
		logger.Println("WARNING: AGENT_TOKEN is unset; agents must use API_TOKEN and then hold an admin credential")
	}

	files, err := execallow.ParseComposeFiles(os.Getenv("ACTIONS_COMPOSE_FILES"))
	if err != nil {
		return err
	}
	srv.actionPolicy.ComposeFiles = files
	if os.Getenv("ACTIONS_ENABLED") == "1" {
		if srv.actionPolicy.Host == "" || srv.operatorToken == "" {
			return fmt.Errorf("ACTIONS_ENABLED=1 requires ACTIONS_HOST and OPERATOR_TOKEN")
		}
		srv.exec = &execallow.Runner{Policy: srv.actionPolicy, Inventory: func() (*inventory.Inventory, error) { return st.Latest(srv.actionPolicy.Host) }}
		srv.actions, err = actions.Open(*dir, srv.exec)
		if err != nil {
			return fmt.Errorf("opening action audit: %w", err)
		}
		logger.Printf("restart actions on: %d commands available", len(execallow.Commands()))
	} else {
		logger.Println("ACTIONS_ENABLED is not 1; restart execution is off")
	}

	minLevel, ok := alert.ParseLevel(*notifyLevel)
	if !ok {
		return fmt.Errorf("-notify-level must be one of: critical, warning, info")
	}
	// The environment is the seed; anything saved from the dashboard's admin
	// page wins over it. See internal/settings for why every stored field is
	// a pointer.
	policy, err := retentionEnv()
	if err != nil {
		return err
	}
	srv.defaults = settings.Defaults{
		RunRetention:     &policy,
		NotifyLevel:      minLevel,
		NotifyInterval:   *notifyEvery,
		DigestInterval:   *digestEvery,
		AppriseURL:       os.Getenv("APPRISE_URL"),
		DigestAppriseURL: os.Getenv("DIGEST_APPRISE_URL"),
	}

	// The notifier is always constructed, even with no endpoint configured:
	// one can now be set from the page, and a nil notifier would mean that
	// change needed a restart to take effect. Whether anything is actually
	// sent is decided per cycle by Options.
	srv.notifier = &notify.Notifier{
		State:    st,
		Sender:   &notify.Apprise{URLFn: func() string { return srv.effective().AppriseURL }, Client: &http.Client{Timeout: 15 * time.Second}},
		Min:      minLevel,
		Log:      logger,
		Recorder: &notify.Recorder{State: st},
		// The digest is a report, not an alarm, and a report has no business
		// competing with an alarm for the same channel. Its own endpoint is
		// resolved per cycle like everything else; when none is set it falls
		// back to the main one, so an unconfigured digest behaves exactly as
		// it did before this field existed.
		DigestSender: &notify.Apprise{
			URLFn: func() string {
				eff := srv.effective()
				if eff.DigestAppriseURL != "" {
					return eff.DigestAppriseURL
				}
				return eff.AppriseURL
			},
			Client: &http.Client{Timeout: 15 * time.Second},
		},
		Options: func() notify.Options {
			eff := srv.effective()
			return notify.Options{Min: eff.NotifyLevel, Enabled: eff.Sending()}
		},
	}

	if eff := srv.effective(); eff.Sending() {
		logger.Printf("notifications on: %s and above, evaluated every %s", eff.NotifyLevel, eff.NotifyInterval)
	} else {
		logger.Println("notifications are off: no apprise endpoint is set. " +
			"Set one in the dashboard's Admin tab, or APPRISE_URL in the environment")
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           withLogging(logger, srv.routes()),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The notification loop lives here rather than in a `alerts --notify`
	// timer because this is the only always-on process and it already has the
	// store. The CLI reads a laptop's inventories/, which is the wrong source
	// of truth for something that should fire at 03:00 whether or not anyone
	// has run `make collect` lately.
	// Started unconditionally and gated inside, since the interval is now a
	// setting: a digest switched on from the page must not need a restart.
	if *digestEvery > 0 {
		logger.Printf("digest on: a summary every %s", *digestEvery)
	} else {
		logger.Println("digest off (DIGEST_INTERVAL=0); it can be turned on from the Admin tab")
	}
	go srv.digestLoop(ctx)
	go srv.pruneLoop(ctx)

	{
		go srv.notifier.Loop(ctx, func() time.Duration { return srv.effective().NotifyInterval }, func() ([]alert.Alert, error) {
			invs, err := st.LatestAll()
			if err != nil {
				return nil, err
			}
			now := time.Now()
			rep, err := srv.evaluateAlerts(invs, now)
			if err != nil {
				return nil, err
			}
			// Silences are applied here rather than inside the notifier, so
			// that "acknowledged" means the same thing on the dashboard and on
			// your phone. An alert silenced while outstanding simply leaves the
			// active set; when the silence lapses it re-enters and notifies
			// again, which is exactly what a temporary acknowledgement should
			// do.
			book, err := srv.silences()
			if err != nil {
				logger.Printf("notify: reading silences: %v", err)
			}
			return silence.Unsilenced(silence.Apply(rep, &book, now).Alerts), nil
		})
	}

	go func() {
		started := time.Now()
		n, err := srv.overviewRuns.Warm(st)
		if err != nil {
			logger.Printf("overview warm: %v", err)
			return
		}
		logger.Printf("overview warm: %d runs in %s", n, time.Since(started).Round(time.Millisecond))
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s, storing under %s", *addr, *dir)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func validateTokens(api, agent, operator string) error {
	tokens := []struct{ name, value string }{{"API_TOKEN", api}, {"AGENT_TOKEN", agent}, {"OPERATOR_TOKEN", operator}}
	for i, token := range tokens {
		if token.value == "" {
			continue
		}
		if len(token.value) < 32 || strings.TrimSpace(token.value) != token.value || strings.ContainsAny(token.value, "\r\n") {
			return fmt.Errorf("%s must be at least 32 characters with no surrounding whitespace or newlines", token.name)
		}
		for _, other := range tokens[:i] {
			if token.value == other.value {
				return fmt.Errorf("%s and %s must differ", token.name, other.name)
			}
		}
	}
	return nil
}

func loopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
