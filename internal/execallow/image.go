package execallow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// composeSetImage replaces one unambiguous image reference atomically.
func (r *Runner) composeSetImage(file, oldImage, newImage string) Result {
	result := Result{Command: "compose-set-image " + file, ExitCode: -1}
	content, err := os.ReadFile(file)
	if err != nil {
		result.Error = fmt.Sprintf("reading compose file: %v", err)
		return result
	}
	if count := strings.Count(string(content), oldImage); count != 1 {
		result.Error = fmt.Sprintf("old_image must occur exactly once (found %d)", count)
		return result
	}
	info, err := os.Stat(file)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".compose-*")
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(info.Mode().Perm()); err == nil {
		_, err = tmp.WriteString(strings.Replace(string(content), oldImage, newImage, 1))
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		result.Error = fmt.Sprintf("replacing compose file: %v", err)
		return result
	}
	result.ExitCode, result.Stdout = 0, oldImage+" -> "+newImage
	return result
}
