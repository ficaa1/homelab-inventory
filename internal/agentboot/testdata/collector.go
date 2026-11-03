// A synthetic executable collector for the real launcher/updater integration
// harness. It is never built into discover or installed by the product.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-list" || os.Args[1] == "-selftest") {
		return
	}
	path, _ := os.Executable()
	b, _ := os.ReadFile(path)
	i := bytes.LastIndex(b, []byte("\nFIXTURE:"))
	mode := "good"
	if i >= 0 {
		mode = string(b[i+9:])
	}
	state := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if _, e := os.Stat(filepath.Join(state, "host-recovered")); e != nil && (mode == "trigger-errors" || mode == "trigger-timeout") {
		_ = os.WriteFile(filepath.Join(state, "host-symptom"), []byte(mode[len("trigger-"):]), 0600)
	}
	if symptom, e := os.ReadFile(filepath.Join(state, "host-symptom")); e == nil {
		mode = string(symptom)
	}
	if mode == "later-errors" || mode == "later-timeout" {
		if _, e := os.Stat(filepath.Join(state, "break-collector")); e == nil {
			mode = mode[len("later-"):]
		}
	}
	switch mode {
	case "silent":
		return
	case "malformed":
		fmt.Print("{")
		return
	case "oversized":
		fmt.Print(string(bytes.Repeat([]byte("x"), (16<<20)+1)))
		return
	case "exit":
		os.Exit(3)
	case "timeout":
		time.Sleep(300 * time.Second)
		return
	case "hung":
		child := exec.Command("/bin/sleep", "300")
		if child.Start() != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(filepath.Join(state, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
		time.Sleep(300 * time.Second)
		return
	case "later":
		if _, e := os.Stat(filepath.Join(state, "break-collector")); e == nil {
			os.Exit(3)
		}
	}
	host, _ := os.Hostname()
	diag := []map[string]string{}
	if mode == "errors" {
		diag = append(diag, map[string]string{"severity": "error", "collector": "fixture", "code": "failed", "message": "synthetic"})
	}
	at := time.Now().UTC()
	if mode == "backward" {
		at = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	data := map[string]any{"schema_version": "5", "collected_at": at, "host": map[string]string{"hostname": host}, "tool": map[string]any{"version": "fixture", "revision": 1, "platform": runtime.GOOS + "-" + runtime.GOARCH}, "docker": nil, "future_field": map[string]int{"n": 7}, "diagnostics": diag}
	if mode == "wrong-build" {
		data["tool"] = map[string]any{"version": "wrong", "revision": 1}
	}
	if mode == "duplicate" {
		fmt.Printf(`{"schema_version":"5","host":{"hostname":%q},"HOST":{"hostname":"other"}}`, host)
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(data)
}
