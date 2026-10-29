package collect

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// SharesCollector reads what this host exports over NFS and SMB.
//
// It reads configuration files rather than running exportfs or testparm. Two
// reasons: it adds no binaries to the read-only allowlist, and it works
// identically on a host where the service is configured but not running —
// which is exactly the stale-configuration case worth finding on a server
// that has accumulated debt.
//
// The trade-off is honest: a config file is what was written, while exportfs
// reports what the kernel actually has. They diverge if someone edited a file
// without reloading. Source records which file each entry came from so that
// divergence is at least attributable.
type SharesCollector struct{}

func (SharesCollector) Name() string { return "shares" }

// nfsExportFiles are the standard locations. TrueNAS generates /etc/exports
// from its middleware, so reading it captures the effective configuration
// without going near the API.
var nfsExportFiles = []string{"/etc/exports"}

var nfsExportDirs = []string{"/etc/exports.d"}

// smbConfigFiles covers stock Samba and TrueNAS, which uses smb4.conf.
var smbConfigFiles = []string{
	"/etc/samba/smb.conf",
	"/etc/smb4.conf",
	"/usr/local/etc/smb4.conf",
}

func (c SharesCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	s := &inventory.Shares{
		NFS: []inventory.NFSExport{},
		SMB: []inventory.SMBShare{},
	}

	paths := append([]string{}, nfsExportFiles...)
	for _, dir := range nfsExportDirs {
		paths = append(paths, listDirFiles(env, dir)...)
	}

	found := false
	for _, path := range paths {
		content, err := env.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				env.Warnf("could not read %s: %v", path, err)
			}
			continue
		}
		found = true
		s.NFS = append(s.NFS, parseExports(content, path)...)
	}

	for _, path := range smbConfigFiles {
		content, err := env.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				env.Warnf("could not read %s: %v", path, err)
			}
			continue
		}
		found = true
		s.SMB = append(s.SMB, parseSMBConf(content, path)...)
		break // the first config that exists is the one in use
	}

	// Absent rather than empty on a host that serves nothing at all, for the
	// same reason as the proxmox and zfs sections.
	if !found {
		return nil
	}
	inv.Shares = s

	sort.Slice(s.NFS, func(a, b int) bool { return s.NFS[a].Path < s.NFS[b].Path })
	sort.Slice(s.SMB, func(a, b int) bool { return s.SMB[a].Name < s.SMB[b].Name })

	for _, e := range s.NFS {
		for _, cl := range e.Clients {
			if cl.Spec == "*" && !cl.ReadOnly {
				env.Warnf("%s is exported read-write to everyone (*)", e.Path)
			}
		}
	}
	env.Infof("%d NFS export(s), %d SMB share(s)", len(s.NFS), len(s.SMB))
	return nil
}

// parseExports reads /etc/exports format:
//
//	/mnt/media/share 192.0.2.24/24(rw,sync,no_subtree_check) 10.0.0.0/8(ro)
//	"/mnt/my share"  *(ro,all_squash)
func parseExports(content, source string) []inventory.NFSExport {
	var out []inventory.NFSExport

	for _, raw := range strings.Split(joinContinuations(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, rest := cutExportPath(line)
		if path == "" {
			continue
		}
		e := inventory.NFSExport{Path: path, Source: source, Clients: []inventory.NFSClient{}}
		for _, tok := range strings.Fields(rest) {
			e.Clients = append(e.Clients, parseNFSClient(tok))
		}
		out = append(out, e)
	}
	return out
}

// joinContinuations folds backslash-continued lines into one logical line.
//
// TrueNAS generates /etc/exports one client per line, continued with a
// trailing backslash:
//
//	/mnt/media/share\
//		192.0.2.10(sec=sys,rw,no_subtree_check)\
//		192.0.2.25(sec=sys,rw,no_subtree_check)
//
// Parsing that line-by-line produced garbage: the path picked up a "\(ro)"
// client, and each continuation became an export whose path was an IP address.
// exports(5) has supported this since forever; the first version simply did
// not anticipate it, which is what reading a real file rather than a
// hand-written fixture catches.
func joinContinuations(content string) string {
	var b strings.Builder
	pending := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if pending {
			// Continuations are indented; collapse that to a single space so
			// the fields split cleanly.
			b.WriteString(" ")
			trimmed = strings.TrimLeft(trimmed, " \t")
		}
		if strings.HasSuffix(trimmed, `\`) {
			b.WriteString(strings.TrimSuffix(trimmed, `\`))
			pending = true
			continue
		}
		b.WriteString(trimmed)
		b.WriteString("\n")
		pending = false
	}
	return b.String()
}

// cutExportPath splits the leading path, which may be double-quoted when it
// contains spaces, from the client list that follows.
func cutExportPath(line string) (path, rest string) {
	if strings.HasPrefix(line, `"`) {
		if end := strings.Index(line[1:], `"`); end >= 0 {
			return line[1 : end+1], strings.TrimSpace(line[end+2:])
		}
		return "", ""
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i:])
	}
	// An export with no client list is valid and means "everyone".
	return line, ""
}

// parseNFSClient reads "192.0.2.24/24(rw,sync)" into its parts.
func parseNFSClient(tok string) inventory.NFSClient {
	c := inventory.NFSClient{Spec: tok}
	open := strings.Index(tok, "(")
	if open < 0 || !strings.HasSuffix(tok, ")") {
		// No options at all: the kernel's default is read-only.
		c.ReadOnly = true
		return c
	}
	c.Spec = tok[:open]
	opts := strings.Split(tok[open+1:len(tok)-1], ",")

	// Default is ro unless rw appears; recording the flag explicitly saves
	// every consumer from having to know that.
	c.ReadOnly = true
	for _, o := range opts {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "rw" {
			c.ReadOnly = false
		}
		if o == "ro" {
			c.ReadOnly = true
		}
		c.Options = append(c.Options, o)
	}
	return c
}

// smbSkipSections are Samba's built-in sections, which are configuration
// rather than shares anyone mounts deliberately.
var smbSkipSections = map[string]bool{
	"global": true, "printers": true, "print$": true, "ipc$": true,
}

// parseSMBConf reads smb.conf's INI-ish format. Keys contain spaces
// ("read only = yes"), values may be quoted, and both ; and # start comments.
func parseSMBConf(content, source string) []inventory.SMBShare {
	var out []inventory.SMBShare
	var current *inventory.SMBShare

	flush := func() {
		if current != nil {
			out = append(out, *current)
			current = nil
		}
	}

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			name := strings.TrimSpace(line[1 : len(line)-1])
			if smbSkipSections[strings.ToLower(name)] {
				continue
			}
			current = &inventory.SMBShare{
				Name:    name,
				Source:  source,
				Options: map[string]string{},
			}
			continue
		}
		if current == nil {
			continue // a key inside [global] or a skipped section
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "path":
			current.Path = value
		case "comment":
			current.Comment = value
		}
		// Everything is kept, redacted: smb.conf holds ldap credentials and
		// passdb settings alongside the harmless flags.
		current.Options[key] = redact.Value(key, value)
	}
	flush()
	return out
}
