package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/packages"
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
