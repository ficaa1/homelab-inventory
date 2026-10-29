package sysexec

import (
	"context"
	"errors"
	"testing"
)

func TestDiskHealthCommandsStayReadOnly(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	read := []string{"-H", "-A", "-j", "-n", "standby", "/dev/sda"}
	for _, tc := range []struct {
		cmd  string
		args []string
		want error
	}{
		{"smartctl", read, ErrNotFound},
		{"smartctl", []string{"--scan"}, ErrNotFound},
		{"journalctl", KernelJournalArgs(), ErrNotFound},
		{"smartctl", []string{"-s", "on", "/dev/sda"}, ErrNotAllowed},
		{"smartctl", []string{"-t", "long", "/dev/sda"}, ErrNotAllowed},
		{"smartctl", []string{"-X"}, ErrNotAllowed},
		{"smartctl", []string{"--set=apm,128", "/dev/sda"}, ErrNotAllowed},
		{"smartctl", []string{"--smart=on", "/dev/sda"}, ErrNotAllowed},
		{"smartctl", []string{"-n", "idle"}, ErrNotAllowed},
		{"smartctl", []string{"-H", "-A", "-j", "/dev/../etc/passwd"}, ErrNotAllowed},
		{"smartctl", append(append([]string{}, read...), "-s", "off"), ErrNotAllowed},
		{"journalctl", []string{"--rotate"}, ErrNotAllowed},
		{"journalctl", []string{"--vacuum-time=1d"}, ErrNotAllowed},
		{"journalctl", []string{"--flush"}, ErrNotAllowed},
		{"journalctl", append(KernelJournalArgs(), "--vacuum-size", "1M"), ErrNotAllowed},
		{"journalctl", []string{"--kernel", "--output=json", "--file=/tmp/log"}, ErrNotAllowed},
	} {
		if _, err := New().Run(context.Background(), tc.cmd, tc.args...); !errors.Is(err, tc.want) {
			t.Errorf("%s %v: got %v want %v", tc.cmd, tc.args, err, tc.want)
		}
	}
}
