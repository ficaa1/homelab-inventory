package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefuseRedirectsStopsTheFollowAndKeepsCredentialsHome(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect followed")
	}))
	defer sink.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusFound)
	}))
	defer origin.Close()

	client := &http.Client{CheckRedirect: RefuseRedirects()}
	_, err := client.Get(origin.URL)
	if err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("err = %v, want redirect refused", err)
	}
}
