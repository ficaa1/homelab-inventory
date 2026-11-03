// Package agentrelease defines the signed, offline-published agent protocol.
package agentrelease

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

const MaxArtifact = 64 << 20

type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Role   string `json:"role,omitempty"`
}
type Manifest struct {
	Product              string              `json:"product,omitempty"`
	LauncherProtocol     int                 `json:"launcher_protocol,omitempty"`
	RuntimeCompatibility int                 `json:"runtime_compatibility,omitempty"`
	Platforms            []string            `json:"platforms"`
	Sequence             uint64              `json:"sequence"`
	Revision             int                 `json:"revision"`
	Version              string              `json:"version"`
	Created              time.Time           `json:"created"`
	Artifacts            map[string]Artifact `json:"artifacts"` // platform/component
}
type Signed struct {
	Manifest  json.RawMessage `json:"manifest"`
	Signature string          `json:"signature"`
}

func Verify(data, public []byte) (Manifest, error) {
	var s Signed
	var m Manifest
	if len(data) > 65536 {
		return m, errors.New("manifest too large")
	}
	if _, err := agentproto.Object(data); err != nil {
		return m, errors.New("invalid envelope")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return m, errors.New("invalid envelope")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(public)))
	sig, e := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil || e != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, s.Manifest, sig) {
		return m, errors.New("invalid manifest signature")
	}
	if _, err = agentproto.Object(s.Manifest); err != nil {
		return m, errors.New("invalid manifest")
	}
	if err = json.Unmarshal(s.Manifest, &m); err != nil {
		return m, errors.New("invalid manifest")
	}
	components := []string{"discover"}
	if m.LauncherProtocol != 0 {
		if m.LauncherProtocol != 1 || m.RuntimeCompatibility != 1 || m.Product != "homelab-inventory" {
			return m, errors.New("unsupported runtime protocol")
		}
		components = []string{"discover", "agentupdate"}
	} else if m.Product != "" || m.RuntimeCompatibility != 0 {
		return m, errors.New("incomplete runtime contract")
	}
	if m.Sequence == 0 || m.Revision < 1 || m.Version == "" || m.Created.IsZero() || len(m.Platforms) == 0 || len(m.Artifacts) != len(m.Platforms)*len(components) {
		return m, errors.New("incomplete manifest")
	}
	if err := validatePlatforms(m.Platforms); err != nil {
		return m, err
	}
	for _, platform := range m.Platforms {
		for _, component := range components {
			a, ok := m.Artifacts[platform+"/"+component]
			if !ok || (m.LauncherProtocol != 0 && a.Role != component) || (m.LauncherProtocol == 0 && a.Role != "") || a.Name != component+"-"+platform || a.Size <= 0 || a.Size > MaxArtifact || len(a.SHA256) != 64 {
				return m, errors.New("invalid artifact metadata")
			}
			if _, err := hex.DecodeString(a.SHA256); err != nil {
				return m, errors.New("invalid artifact digest")
			}
		}
	}
	return m, nil
}
func Digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func Check(data []byte, a Artifact) error {
	if int64(len(data)) != a.Size || Digest(data) != a.SHA256 {
		return errors.New("artifact checksum or size mismatch")
	}
	return nil
}
func WriteJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	// Public release metadata contains only sequence/mode, never credentials.
	return fsx.WriteAtomic(path, b, 0644)
}

// Require signer ownership as well as mode 0600; other owners must not be
// able to rewrite a nominally private signing key.
func privateKeyInfoOK(st os.FileInfo, uid uint32) bool {
	owner, ok := st.Sys().(*syscall.Stat_t)
	return ok && owner.Uid == uid && st.Mode().IsRegular() && st.Mode().Perm() == 0600
}
func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !privateKeyInfoOK(st, uint32(os.Geteuid())) {
		return nil, errors.New("private key must be a signer-owned regular file with mode 0600")
	}
	key, err := io.ReadAll(io.LimitReader(f, ed25519.PrivateKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])) {
		return nil, errors.New("invalid private key")
	}
	return ed25519.PrivateKey(key), nil
}

// Keygen never truncates an existing private key and emits only its public half.
func Keygen(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		key, err = readPrivateKey(path)
		if err != nil {
			return nil, err
		}
		pub = ed25519.PrivateKey(key).Public().(ed25519.PublicKey)
	} else {
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return nil, err
		}
	}
	return []byte(base64.StdEncoding.EncodeToString(pub) + "\n"), nil
}

// PublishRuntime emits a separate protocol-1 pair alongside the unchanged legacy
// manifest. Keeping latest.json discover-only lets already installed verifiers
// continue working without trusting new artifact roles.
func PublishRuntime(dir, keyPath, input, version string, revision int) error {
	return publish(dir, keyPath, input, version, revision, true)
}

// Publish is called only by the offline signer, never by the HTTP server.
func Publish(dir, keyPath, input, version string, revision int) error {
	return publish(dir, keyPath, input, version, revision, false)
}
func publish(dir, keyPath, input, version string, revision int, pair bool) error {
	key, err := readPrivateKey(keyPath)
	if err != nil {
		return err
	}
	pub := []byte(base64.StdEncoding.EncodeToString(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)) + "\n")
	var seq uint64 = 1
	if old, err := os.ReadFile(filepath.Join(dir, "latest.json")); err == nil {
		m, e := Verify(old, pub)
		if e != nil {
			return e
		}
		seq = m.Sequence + 1
		if seq == 0 {
			return errors.New("sequence exhausted")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	release := filepath.Join(dir, strconv.FormatUint(seq, 10))
	staging, err := os.MkdirTemp(dir, ".release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = os.Chmod(staging, 0755); err != nil {
		return err
	}
	platforms, err := Platforms(os.Getenv("AGENT_PLATFORMS"))
	if err != nil {
		return err
	}
	m := Manifest{Platforms: platforms, Sequence: seq, Revision: revision, Version: version, Created: time.Now().UTC(), Artifacts: map[string]Artifact{}}
	components := []string{"discover"}
	if pair {
		components = append(components, "agentupdate")
		m.Product = "homelab-inventory"
		m.LauncherProtocol = 1
		m.RuntimeCompatibility = 1
	}
	for _, platform := range platforms {
		for _, component := range components {
			name := component + "-" + platform
			source := filepath.Join(input, name)
			data, e := os.ReadFile(source)
			if e != nil {
				return e
			}
			if len(data) == 0 || len(data) > MaxArtifact {
				return errors.New("artifact size invalid")
			}
			if e = fsx.WriteAtomic(filepath.Join(staging, name), data, 0755); e != nil {
				return e
			}
			a := Artifact{Name: name, SHA256: Digest(data), Size: int64(len(data))}
			if pair {
				a.Role = component
			}
			m.Artifacts[platform+"/"+component] = a
		}
	}
	signed, err := sign(m, key)
	if err != nil {
		return err
	}
	var runtimeSigned []byte
	if pair {
		runtimeSigned = signed
		if err = fsx.WriteAtomic(filepath.Join(staging, "runtime.json"), runtimeSigned, 0644); err != nil {
			return err
		}
		legacy := m
		legacy.Product = ""
		legacy.LauncherProtocol = 0
		legacy.RuntimeCompatibility = 0
		legacy.Artifacts = map[string]Artifact{}
		for _, p := range platforms {
			a := m.Artifacts[p+"/discover"]
			a.Role = ""
			legacy.Artifacts[p+"/discover"] = a
		}
		signed, err = sign(legacy, key)
		if err != nil {
			return err
		}
	}
	if err = fsx.WriteAtomic(filepath.Join(dir, "public.key"), pub, 0644); err != nil {
		return err
	}
	if err = fsx.WriteAtomic(filepath.Join(staging, "manifest.json"), signed, 0644); err != nil {
		return err
	}
	// Install immutable artifacts only after the complete release has validated.
	if _, err = os.Stat(release); err == nil {
		return errors.New("release sequence directory already exists; recover interrupted publication before retrying")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Rename(staging, release); err != nil {
		return err
	}
	if err = syncDirectory(dir); err != nil {
		return err
	}
	// Reset controls before publishing a new sequence. No directory is HTTP-writable.
	if err = WriteJSON(filepath.Join(dir, "control.json"), Control{Sequence: seq, Mode: "auto"}); err != nil {
		return err
	}
	// Readers obtain runtime.json from the retained directory selected by the
	// legacy latest pointer; there is no second mutable latest pointer to race.
	return fsx.WriteAtomic(filepath.Join(dir, "latest.json"), signed, 0644)
}

type Control struct {
	Sequence uint64 `json:"sequence"`
	Mode     string `json:"mode"`
}

func Read(dir string) (Manifest, []byte, Control, error) {
	var c Control
	data, err := os.ReadFile(filepath.Join(dir, "latest.json"))
	if err != nil {
		return Manifest{}, nil, c, err
	}
	pub, err := os.ReadFile(filepath.Join(dir, "public.key"))
	if err != nil {
		return Manifest{}, nil, c, err
	}
	m, err := Verify(data, pub)
	if err != nil {
		return m, nil, c, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return m, data, c, err
}

// Resign rotates the envelope trust root without changing executable hashes or
// sequence. Caller holds the signer lock and has stopped fleet schedules.
func Resign(dir, keyPath string) error {
	m, data, c, err := Read(dir)
	if err != nil {
		return err
	}
	if c.Mode != "hold" || c.Sequence != m.Sequence {
		return errors.New("rotation requires hold")
	}
	key, err := readPrivateKey(keyPath)
	if err != nil {
		return err
	}
	var s Signed
	if err = json.Unmarshal(data, &s); err != nil {
		return err
	}
	s.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(key), s.Manifest))
	data, err = json.Marshal(s)
	if err != nil {
		return err
	}
	pub := []byte(base64.StdEncoding.EncodeToString(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)) + "\n")
	// A crash between files fails closed while held; rerun with restored old
	// public.key/latest.json if necessary. Private keys remain untouched.
	if err = fsx.WriteAtomic(filepath.Join(dir, strconv.FormatUint(m.Sequence, 10), "manifest.json"), data, 0644); err != nil {
		return err
	}
	runtimePath := filepath.Join(dir, strconv.FormatUint(m.Sequence, 10), "runtime.json")
	if rb, e := os.ReadFile(runtimePath); e == nil {
		oldPub, e := os.ReadFile(filepath.Join(dir, "public.key"))
		if e != nil {
			return e
		}
		rm, e := Verify(rb, oldPub)
		if e != nil {
			return e
		}
		rb, e = sign(rm, key)
		if e != nil {
			return e
		}
		if e = fsx.WriteAtomic(runtimePath, rb, 0644); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if err = fsx.WriteAtomic(filepath.Join(dir, "latest.json"), data, 0644); err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(dir, "public.key"), pub, 0644)
}

// Platforms is the build/signing contract. The signed list travels with each
// manifest, so existing pinned updaters need no configuration change.
func Platforms(value string) ([]string, error) {
	if value == "" {
		value = "linux/amd64,linux/arm64"
	}
	out := strings.Split(strings.ReplaceAll(value, "/", "-"), ",")
	return out, validatePlatforms(out)
}
func validatePlatforms(platforms []string) error {
	seen := map[string]bool{}
	if len(platforms) == 0 {
		return errors.New("platform list is empty")
	}
	for _, p := range platforms {
		if (p != "linux-amd64" && p != "linux-arm64" && p != "freebsd-amd64") || seen[p] {
			return errors.New("unsupported or duplicate platform")
		}
		seen[p] = true
	}
	return nil
}

func sign(m Manifest, key ed25519.PrivateKey) ([]byte, error) {
	b, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	return json.Marshal(Signed{Manifest: b, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))})
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// ReadSequence verifies the retained exact envelope before exposing any names.
func ReadSequence(dir string, seq uint64, pair bool) (Manifest, []byte, error) {
	name := "manifest.json"
	if pair {
		name = "runtime.json"
	}
	data, e := os.ReadFile(filepath.Join(dir, strconv.FormatUint(seq, 10), name))
	if e != nil {
		return Manifest{}, nil, e
	}
	pub, e := os.ReadFile(filepath.Join(dir, "public.key"))
	if e != nil {
		return Manifest{}, nil, e
	}
	m, e := Verify(data, pub)
	if e == nil && (m.Sequence != seq || pair && m.LauncherProtocol != 1) {
		e = errors.New("retained manifest identity mismatch")
	}
	return m, data, e
}
