package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/packages"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/softstore"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func (s *server) handleHostPackages(w http.ResponseWriter, r *http.Request) {
	q := packages.Query{Q: r.URL.Query().Get("q"), Scope: r.URL.Query().Get("scope"), Limit: 200}
	for key, dest := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset} {
		if raw := r.URL.Query().Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (key == "limit" && n == 0) {
				writeError(w, http.StatusBadRequest, key+" must be a positive limit or nonnegative offset")
				return
			}
			*dest = n
		}
	}
	name := r.PathValue("name")
	_, err := s.store.History(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	out, err := s.packageStore().Host(name, q)
	if err != nil {
		s.log.Printf("packages: %s: %v", name, err)
		writeError(w, 500, "could not load package inventory")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *server) handlePackageSearch(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || strings.Contains(strings.TrimSuffix(name, "*"), "*") {
		writeError(w, http.StatusBadRequest, "name must be an exact package name or a prefix ending in *")
		return
	}
	hosts, err := s.store.Hosts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load inventories")
		return
	}
	// Hosts lists directories, including auxiliary stores. Include only hosts
	// with stored runs, matching LatestAll without loading inventory bodies.
	known := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if _, err := s.store.History(host); errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "could not load inventories")
			return
		}
		known = append(known, host)
	}
	out, err := s.packageStore().Search(known, name)
	if err != nil {
		s.log.Printf("packages: search: %v", err)
		writeError(w, 500, "could not load package inventories")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) packageStore() *softstore.Store {
	s.softwareOnce.Do(func() { s.software = softstore.New(s.store.Dir()) })
	return s.software
}
func (s *server) handlePackageChanges(w http.ResponseWriter, r *http.Request) {
	limit := 30
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, 400, "limit must be positive")
			return
		}
		limit = n
	}
	name := r.PathValue("name")
	if _, err := s.store.History(name); err != nil {
		s.respondLookup(w, name, err)
		return
	}
	out, err := s.packageStore().Changes(name, limit)
	if err != nil {
		s.log.Printf("packages: changes for %s: %v", name, err)
		writeError(w, 500, "could not load package changes")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// pluginPackageHosts passes only package facts through the plugin boundary.
func (s *server) pluginPackageHosts() ([]plugin.PackageHost, error) {
	hosts, err := s.store.Hosts()
	if err != nil {
		return nil, err
	}
	out := []plugin.PackageHost{}
	for _, host := range hosts {
		if _, err := s.store.History(host); errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		sets, at, err := s.packageStore().FullSets(host)
		if err != nil {
			return nil, err
		}
		h := plugin.PackageHost{Host: host, CollectedAt: at}
		if sets != nil {
			h.Sets = []plugin.PackageSet{}
		}
		for _, set := range sets {
			p := plugin.PackageSet{Scope: set.Scope, ImageRef: set.ImageRef, Containers: append([]string{}, set.Containers...), Manager: set.Manager, Status: set.Status, Distro: plugin.PackageDistro{ID: set.Distro.ID, VersionID: set.Distro.VersionID, Codename: set.Distro.Codename}}
			if set.Packages != nil {
				p.Packages = []plugin.Package{}
			}
			for _, row := range set.Packages {
				p.Packages = append(p.Packages, plugin.Package{Name: row.Name, Version: row.Version, Arch: row.Arch, Source: row.Source, SourceVersion: row.SourceVersion})
			}
			h.Sets = append(h.Sets, p)
		}
		out = append(out, h)
	}
	return out, nil
}
