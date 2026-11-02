package archivetier

import "testing"

func TestSeedDir(t *testing.T) {
	for in, want := range map[string]string{
		"/data/media/series/Twin Peaks/": "/data/media/series/Twin Peaks",
		"/data/media/movies":             "/data/media/movies",
		"/data/media/movies//":           "/data/media/movies",
		"/":                              "/",
		"":                               "",
	} {
		if got := seedDir(in); got != want {
			t.Errorf("seedDir(%q) = %q, want %q", in, got, want)
		}
	}
}
