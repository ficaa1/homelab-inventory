package plugin

import "net/http"

// RequestObserver may record a configured tripwire. It must never authorize a
// request; a true result tells core to refuse the request before normal auth.
type RequestObserver interface{ Observe(*http.Request) bool }

func (r *Registry) ObserveRequest(req *http.Request) bool {
	tripped := false
	for _, id := range r.ids() {
		e := r.entries[id]
		p, ok := e.plugin.(RequestObserver)
		if !ok {
			continue
		}
		e.op.Lock()
		if e.ready && e.stored.Enabled {
			tripped = p.Observe(req) || tripped
		}
		e.op.Unlock()
	}
	return tripped
}

// ExcludeCanaryCredentials prevents fake-token observers claiming a real
// credential. Core must refresh this whenever authentication settings change.
func (r *Registry) ExcludeCanaryCredentials(tokens ...string) {
	for _, id := range r.ids() {
		e := r.entries[id]
		p, ok := e.plugin.(interface{ ExcludeCredentials(...string) })
		if !ok {
			continue
		}
		e.op.Lock()
		p.ExcludeCredentials(tokens...)
		e.op.Unlock()
	}
}
