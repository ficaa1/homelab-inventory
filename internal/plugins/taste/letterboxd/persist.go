package letterboxd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"syscall"
)

func profileLock(path string) (func(), error) {
	f, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		f.Close()
		return nil, e
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func Save(path string, p Profile) error {
	unlock, e := profileLock(path)
	if e != nil {
		return e
	}
	defer unlock()
	return writeProfile(path, p)
}
func saveIfCurrent(path string, p Profile, before [32]byte) error {
	unlock, e := profileLock(path)
	if e != nil {
		return e
	}
	defer unlock()
	b, e := os.ReadFile(path)
	if e != nil || sha256.Sum256(b) != before {
		return fmt.Errorf("baseline changed during sync")
	}
	return writeProfile(path, p)
}
