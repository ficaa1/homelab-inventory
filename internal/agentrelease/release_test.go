package agentrelease

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T) (string, string, Manifest, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(t.TempDir(), "key")
	pub, e := Keygen(key)
	if e != nil {
		t.Fatal(e)
	}
	input := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		for _, c := range []string{"discover"} {
			if e = os.WriteFile(filepath.Join(input, c+"-"+p), []byte("payload"), 0755); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e = Publish(dir, key, input, "build-one", 5); e != nil {
		t.Fatal(e)
	}
	m, b, _, e := Read(dir)
	if e != nil {
		t.Fatal(e)
	}
	return dir, key, m, b, pub
}
func TestSignatureChecksumAndShape(t *testing.T) {
	_, _, m, b, pub := fixture(t)
	if _, e := Verify(b, pub); e != nil {
		t.Fatal(e)
	}
	var s Signed
	json.Unmarshal(b, &s)
	s.Manifest = append([]byte(nil), s.Manifest...)
	s.Manifest[10] ^= 1
	bad, _ := json.Marshal(s)
	if _, e := Verify(bad, pub); e == nil {
		t.Fatal("tampered manifest accepted")
	}
	if _, e := Verify(b, []byte("wrong key")); e == nil {
		t.Fatal("wrong key accepted")
	}
	a := m.Artifacts["linux-amd64/discover"]
	if e := Check([]byte("payload"), a); e != nil {
		t.Fatal(e)
	}
	if e := Check([]byte("corrupt"), a); e == nil {
		t.Fatal("corrupt artifact accepted")
	}
}
func TestKeygenNeverOverwritesAndSequence(t *testing.T) {
	dir, key, m, _, pub := fixture(t)
	before, _ := os.ReadFile(key)
	again, e := Keygen(key)
	if e != nil || string(again) != string(pub) {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(key)
	if string(after) != string(before) {
		t.Fatal("key overwritten")
	}
	controlInfo, err := os.Stat(filepath.Join(dir, "control.json"))
	if err != nil || controlInfo.Mode().Perm() != 0644 {
		t.Fatal("public control permissions")
	}
	st, _ := os.Stat(key)
	if st.Mode().Perm() != 0600 {
		t.Fatal("private key permissions")
	}
	input := t.TempDir()
	for _, a := range m.Artifacts {
		data, _ := os.ReadFile(filepath.Join(dir, "1", a.Name))
		name := a.Name
		os.WriteFile(filepath.Join(input, name), data, 0755)
	}
	if e = Publish(dir, key, input, "build-two", 5); e != nil {
		t.Fatal(e)
	}
	m, _, _, e = Read(dir)
	if e != nil || m.Sequence != 2 {
		t.Fatal(m, e)
	}
}
func TestResignRequiresHoldAndPreservesSequence(t *testing.T) {
	dir, _, before, _, oldPub := fixture(t)
	key := filepath.Join(t.TempDir(), "next")
	newPub, e := Keygen(key)
	if e != nil {
		t.Fatal(e)
	}
	if e = Resign(dir, key); e == nil {
		t.Fatal("rotation without hold accepted")
	}
	if e = WriteJSON(filepath.Join(dir, "control.json"), Control{Sequence: before.Sequence, Mode: "hold"}); e != nil {
		t.Fatal(e)
	}
	if e = Resign(dir, key); e != nil {
		t.Fatal(e)
	}
	after, data, c, e := Read(dir)
	if e != nil || after.Sequence != before.Sequence || c.Mode != "hold" {
		t.Fatal(after, c, e)
	}
	if _, e = Verify(data, newPub); e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(data, oldPub); e == nil {
		t.Fatal("old key accepted rotated envelope")
	}
	for name, a := range before.Artifacts {
		if a != after.Artifacts[name] {
			t.Fatal("rotation changed executable", name)
		}
	}
}

type keyInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i keyInfo) Name() string       { return "key" }
func (i keyInfo) Size() int64        { return 64 }
func (i keyInfo) Mode() os.FileMode  { return i.mode }
func (i keyInfo) ModTime() time.Time { return time.Time{} }
func (i keyInfo) IsDir() bool        { return i.mode.IsDir() }
func (i keyInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }
func TestPrivateKeyOwnershipAndRegularFile(t *testing.T) {
	for _, tc := range []struct {
		info keyInfo
		want bool
	}{
		{keyInfo{0600, 0}, true}, {keyInfo{0600, 1000}, false},
		{keyInfo{0640, 0}, false}, {keyInfo{os.ModeNamedPipe | 0600, 0}, false},
	} {
		if got := privateKeyInfoOK(tc.info, 0); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}
func TestSignerRejectsSymlinkAndInsecureKey(t *testing.T) {
	dir, key, _, _, _ := fixture(t)
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(key, link); e != nil {
		t.Fatal(e)
	}
	for _, candidate := range []string{link, key} {
		if candidate == key {
			if e := os.Chmod(key, 0644); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := Keygen(candidate); e == nil {
			t.Fatal("keygen accepted unsafe key")
		}
		if e := Publish(dir, candidate, t.TempDir(), "bad", 5); e == nil {
			t.Fatal("publish accepted unsafe key")
		}
		if e := WriteJSON(filepath.Join(dir, "control.json"), Control{Sequence: 1, Mode: "hold"}); e != nil {
			t.Fatal(e)
		}
		if e := Resign(dir, candidate); e == nil {
			t.Fatal("resign accepted unsafe key")
		}
	}
}

func TestSignedArtifactNameRejectsTraversal(t *testing.T) {
	_, keyPath, m, _, pub := fixture(t)
	key, err := readPrivateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../discover-linux-amd64", "../../root/key", "/discover-linux-amd64", "discover-linux-amd64\\other"} {
		a := m.Artifacts["linux-amd64/discover"]
		a.Name = name
		m.Artifacts["linux-amd64/discover"] = a
		raw, _ := json.Marshal(m)
		envelope, _ := json.Marshal(Signed{Manifest: raw, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))})
		if _, err := Verify(envelope, pub); err == nil {
			t.Fatal("signed traversal accepted", name)
		}
	}
}

func TestConfiguredPlatformContract(t *testing.T) {
	t.Setenv("AGENT_PLATFORMS", "linux/amd64")
	_, _, m, b, pub := fixture(t)
	if len(m.Platforms) != 1 || len(m.Artifacts) != 1 {
		t.Fatal("platform contract ignored")
	}
	// Verification follows the signed contract, independent of the API environment.
	t.Setenv("AGENT_PLATFORMS", "linux/arm64")
	if _, err := Verify(b, pub); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"linux/amd64,linux/amd64", "../../x", "linux/mips", "linux/amd64,"} {
		if _, err := Platforms(value); err == nil {
			t.Fatal("invalid platforms accepted")
		}
	}
}

func TestIncompletePublicationCanRetry(t *testing.T) {
	dir, key, m, _, _ := fixture(t)
	input := t.TempDir()
	if err := Publish(dir, key, input, "next", 5); err == nil {
		t.Fatal("missing artifact accepted")
	}
	for _, a := range m.Artifacts {
		data, err := os.ReadFile(filepath.Join(dir, "1", a.Name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(input, a.Name), data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Publish(dir, key, input, "next", 5); err != nil {
		t.Fatal("incomplete publication blocked retry", err)
	}
}
