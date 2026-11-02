//go:build !linux

package archivetier

import "fmt"

func PurgeOriginals(j Job, mounts []MountMapping, apply bool) ([]PurgeFile, error) {
	return []PurgeFile{}, fmt.Errorf("deleting originals requires Linux")
}
func RestoreSidecars(j Job, mounts []MountMapping, roots []string, apply bool) []SidecarRestore {
	out := []SidecarRestore{}
	for _, sc := range j.Sidecars {
		out = append(out, SidecarRestore{Staged: sc.Staged, Note: "subtitle restore requires Linux"})
	}
	return out
}
func carriedSidecars(j Job, current Target) []string { return nil }
