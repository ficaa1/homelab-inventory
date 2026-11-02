package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
	"github.com/ficaa1/homelab-inventory/internal/mediapolicy"
	"github.com/ficaa1/homelab-inventory/internal/notify"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func readConfig(path string) (mediapolicy.Config, error) {
	c := mediapolicy.DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("cannot open media policy configuration")
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid media policy configuration")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("configuration must contain one JSON document")
	}
	return c, c.Validate()
}
func run() error {
	apply := flag.Bool("apply", false, "explicitly allow policy writes; default is dry-run")
	once := flag.Bool("once", false, "one cycle, print status and exit")
	config := flag.String("config", "media-policy.json", "one non-secret JSON configuration")
	dir := flag.String("state-dir", "./media-policy-state", "durable state and audit directory")
	addr := flag.String("listen", ":8792", "authenticated status and health address")
	interval := flag.Duration("interval", 2*time.Minute, "policy cycle interval")
	noSearch := flag.Bool("no-release-search", false, "prohibit tracker release searches at the HTTP boundary")
	restore := flag.Bool("restore", false, "restore the persisted archive job's originals from the recycle bin, then exit (service must be stopped; dry-run unless -apply)")
	restoreJob := flag.String("restore-job", "", "job ID to restore when several failed archive jobs exist")
	restoreMap := flag.String("restore-map", "", "writable appRoot=localRoot mappings for -restore, comma separated (e.g. /data/media=/data/media)")
	quarantine := flag.String("quarantine", "", "for -restore: replacement files (app paths, comma separated) to move aside first")
	quarantineDir := flag.String("quarantine-dir", "", "for -restore: app-path directory the replacement files move to")
	rescan := flag.Bool("rescan", true, "for -restore: ask the application to rescan the title after restoring (a write; needs -apply)")
	flag.Parse()
	if *restore {
		return runRestore(*config, *dir, *restoreJob, *restoreMap, *quarantine, *quarantineDir, *apply, *rescan)
	}
	if *interval < time.Minute || *interval > time.Hour {
		return fmt.Errorf("interval must be 1m..1h")
	}
	c, err := readConfig(*config)
	if err != nil {
		return err
	}
	store, err := mediapolicy.OpenStore(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	sonarrURL, radarrURL, tautulliURL := os.Getenv("SONARR_URL"), os.Getenv("RADARR_URL"), os.Getenv("TAUTULLI_URL")
	sonarrKey, radarrKey, tautulliKey := os.Getenv("SONARR_API_KEY"), os.Getenv("RADARR_API_KEY"), os.Getenv("TAUTULLI_API_KEY")
	if (c.ArchiveEnabled || c.Seeding.Enabled) && (radarrURL == "" || radarrKey == "" || sonarrURL == "" || sonarrKey == "" || tautulliURL == "" || tautulliKey == "") {
		return fmt.Errorf("Sonarr, Radarr and Tautulli must be configured for archive or seeding")
	}
	client := mediapolicy.SharedHTTP(&mediapolicy.Transport{Apply: *apply, Store: store, SonarrOrigin: sonarrURL, RadarrOrigin: radarrURL, QbitOrigin: os.Getenv("QBIT_URL"), NoSearch: *noSearch})
	svc := &mediapolicy.Service{Config: c, Store: store, DryRun: !*apply, Interval: *interval, LibraryClients: map[string]*archivetier.Client{}}
	for _, v := range []struct{ kind, base, key string }{{"sonarr", sonarrURL, sonarrKey}, {"radarr", radarrURL, radarrKey}} {
		if v.base == "" || v.key == "" {
			continue
		}
		a := archivetier.NewClient(v.kind, v.base, v.key, *apply)
		a.HTTP, a.Safety = client, c.Archive
		svc.LibraryClients[v.kind] = a
	}
	if c.ArchiveEnabled {
		archiveStore := &archivetier.Store{Dir: *dir, LoadState: store.LoadArchive, SaveState: store.SaveArchive, AuditRecord: store.Audit, AuditDetailRecord: store.AuditDetail, Writable: store.WriteAllowed}
		r := &archivetier.Runner{Interval: *interval, Config: c.Archive, Store: archiveStore, DryRun: !*apply, NoSearch: *noSearch, Now: time.Now, Arr: map[string]archivetier.ArrAPI{}, Acquire: func(t archivetier.Target) (func(), error) { return store.Acquire(t.Key(), "archive") }, RecycleCheck: c.Archive.RecycleCheck}
		for kind, a := range svc.LibraryClients {
			r.Arr[kind] = a
		}
		plex := archivetier.NewPlex(tautulliURL, tautulliKey)
		plex.HTTP = client
		r.Plex = plex
		r.DState = func() (int, error) { return archivetier.CountDState(c.Archive.ProcRoot) }
		r.DownloadFree = c.Archive.DownloadFree
		svc.DownloadFree = c.Archive.DownloadFree
		r.LogSummary = func(summary string) { log.Print(summary) }
		if endpoint := os.Getenv("APPRISE_URL"); endpoint != "" {
			sender := notify.NewApprise(endpoint)
			sender.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("notification redirect refused") }
			r.Notify = func(ctx context.Context, body string) error {
				return sender.Send(ctx, notify.Message{Title: "Media archive", Body: body, Type: "info"})
			}
		}
		svc.Archive = r
	}
	if c.Seeding.Enabled {
		if os.Getenv("QBIT_URL") == "" || os.Getenv("QBIT_USER") == "" || os.Getenv("QBIT_PASSWORD") == "" {
			return fmt.Errorf("qBittorrent URL, user and password required for seeding")
		}
		plex := archivetier.NewPlex(tautulliURL, tautulliKey)
		plex.HTTP = client
		svc.Seeding = &mediapolicy.SeedMover{Qbit: mediapolicy.NewSeedQbit(os.Getenv("QBIT_URL"), os.Getenv("QBIT_USER"), os.Getenv("QBIT_PASSWORD"), client), Active: plex.Active}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer func() { stop(); svc.WaitSeeding() }()
	if svc.Seeding != nil {
		svc.Seeding.Context = ctx
	}
	tick := func() error {
		tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		return svc.Tick(tctx, time.Now().UTC())
	}
	if *once {
		err := tick()
		json.NewEncoder(os.Stdout).Encode(svc.Status())
		return err
	}
	token := os.Getenv("MEDIA_POLICY_TOKEN")
	if token == "" {
		return fmt.Errorf("MEDIA_POLICY_TOKEN required in daemon mode")
	}
	server := &http.Server{Addr: *addr, Handler: svc.Handler(token), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()
	if svc.ResumeSeedQueue() {
		log.Print("seeding queue resumed")
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	runNow := false
	for {
		if !runNow {
			if err := tick(); err != nil {
				log.Print(err)
			}
		}
		runNow = false
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("media policy status listener failed")
			}
			return nil
		case <-ticker.C:
		case <-svc.Wake():
			// The operator's "run now": one cycle with the nightly window
			// ignored, then back to the normal interval.
			rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			if err := svc.RunNowTick(rctx, time.Now().UTC()); err != nil {
				log.Print(err)
			}
			cancel()
			ticker.Reset(*interval)
			runNow = true
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// runRestore is the operator restore of the persisted archive job. It takes
// the state lock, so it refuses to run while the service is running, and it
// leaves the job in state: clearing it is a separate, reviewed step.
func runRestore(config, dir, jobID, mapping, quarantine, quarantineDir string, apply, rescan bool) error {
	c, err := readConfig(config)
	if err != nil {
		return err
	}
	store, err := mediapolicy.OpenStore(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	job, err := restoreJob(store.Snapshot().Archive, jobID)
	if err != nil {
		return err
	}
	r := &archivetier.Restorer{LibraryRoots: c.Archive.LibraryRoots, Quarantine: splitList(quarantine), QuarantineRoot: quarantineDir, Apply: apply, Rescan: rescan}
	for _, m := range splitList(mapping) {
		app, local, ok := strings.Cut(m, "=")
		if !ok {
			return fmt.Errorf("-restore-map wants appRoot=localRoot")
		}
		r.Mounts = append(r.Mounts, archivetier.MountMapping{AppRoot: app, LocalRoot: local})
	}
	if len(r.Mounts) == 0 {
		return fmt.Errorf("-restore needs -restore-map")
	}
	if rescan {
		kind := job.Target.Kind
		base, key := os.Getenv(strings.ToUpper(kind)+"_URL"), os.Getenv(strings.ToUpper(kind)+"_API_KEY")
		if apply && (base == "" || key == "") {
			return fmt.Errorf("%s URL and key required for the rescan; or pass -rescan=false", kind)
		}
		t := &mediapolicy.Transport{Apply: apply, Store: store}
		if kind == "sonarr" {
			t.SonarrOrigin = base
		} else {
			t.RadarrOrigin = base
		}
		client := archivetier.NewClient(kind, base, key, apply)
		client.HTTP = mediapolicy.SharedHTTP(t)
		r.Client = client
	}
	res, err := r.Restore(context.Background(), *job)
	json.NewEncoder(os.Stdout).Encode(res)
	if err != nil {
		return err
	}
	if apply {
		return store.Audit("restore_applied", job)
	}
	return nil
}

func restoreJob(st archivetier.State, id string) (*archivetier.Job, error) {
	if id != "" {
		if j := st.FindJob(id); j != nil {
			return j, nil
		}
		return nil, archivetier.ErrUnknownJob
	}
	if len(st.FailedJobs) == 1 {
		return st.FailedJobs[0], nil
	}
	if len(st.FailedJobs) > 1 {
		return nil, fmt.Errorf("several failed jobs; select -restore-job using the displayed job ID")
	}
	if st.Job != nil {
		return st.Job, nil
	}
	return nil, fmt.Errorf("no persisted archive job to restore")
}
