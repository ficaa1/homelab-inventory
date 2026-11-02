//go:build !linux

package archivetier

import "fmt"

func StageSidecars(j *Job, c Config, stateDir string) error {
	if len(c.SidecarMounts) == 0 {
		j.SidecarMessage = "subtitles not carried over (sidecarMounts unset)"
		return nil
	}
	return fmt.Errorf("subtitle carry-over requires Linux")
}
func CarrySidecars(j *Job, current Target, c Config) error {
	return fmt.Errorf("subtitle carry-over requires Linux")
}
