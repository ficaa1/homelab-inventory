// Package passkeytest is a software authenticator for tests: it answers
// WebAuthn create and get ceremonies the way a browser and a passkey would.
package passkeytest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
)

// COSE algorithm identifiers.
const (
	ES256 = -7
	EdDSA = -8
	RS256 = -257
)

// Authenticator holds one credential. The exported fields may be changed
// between ceremonies to produce answers a relying party must refuse.
type Authenticator struct {
	Origin  string // what the browser reports as the page origin
	RPID    string // hashed into authenticator data; the origin's host by default
	ID      []byte
	Alg     int
	Flags   byte   // user present and verified by default
	Counter uint32 // sent as is; a synced passkey keeps it at zero
	key     crypto.Signer
}

func New(t testing.TB, origin string, alg int) *Authenticator {
	t.Helper()
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{Origin: origin, RPID: u.Hostname(), ID: make([]byte, 32), Alg: alg, Flags: 0x05}
	rand.Read(a.ID)
	switch alg {
	case ES256:
		a.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case EdDSA:
		_, a.key, err = ed25519.GenerateKey(rand.Reader)
	case RS256:
		a.key, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		t.Fatalf("unknown algorithm %d", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// CredentialID is the base64url form a page sends as the credential's id.
func (a *Authenticator) CredentialID() string { return base64.RawURLEncoding.EncodeToString(a.ID) }

func head(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
	}
	return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(n))
}
func cborInt(n int64) []byte {
	if n < 0 {
		return head(1, uint64(-1-n))
	}
	return head(0, uint64(n))
}
func cborBytes(b []byte) []byte { return append(head(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(head(3, uint64(len(s))), s...) }
func cborMap(pairs ...[]byte) []byte {
	out := head(5, uint64(len(pairs)/2))
	for _, p := range pairs {
		out = append(out, p...)
	}
	return out
}

func (a *Authenticator) coseKey() []byte {
	switch k := a.key.Public().(type) {
	case *ecdsa.PublicKey:
		raw, _ := k.Bytes() // 0x04 || X || Y
		return cborMap(cborInt(1), cborInt(2), cborInt(3), cborInt(ES256), cborInt(-1), cborInt(1), cborInt(-2), cborBytes(raw[1:33]), cborInt(-3), cborBytes(raw[33:]))
	case ed25519.PublicKey:
		return cborMap(cborInt(1), cborInt(1), cborInt(3), cborInt(EdDSA), cborInt(-1), cborInt(6), cborInt(-2), cborBytes(k))
	case *rsa.PublicKey:
		return cborMap(cborInt(1), cborInt(3), cborInt(3), cborInt(RS256), cborInt(-1), cborBytes(k.N.Bytes()), cborInt(-2), cborBytes(big.NewInt(int64(k.E)).Bytes()))
	}
	return nil
}

func (a *Authenticator) clientData(kind, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
	return b
}
func (a *Authenticator) authData(flags byte) []byte {
	hash := sha256.Sum256([]byte(a.RPID))
	return binary.BigEndian.AppendUint32(append(hash[:], flags), a.Counter)
}

// Create answers navigator.credentials.create() with "none" attestation.
func (a *Authenticator) Create(challenge string) (clientDataJSON, attestationObject []byte) {
	data := a.authData(a.Flags | 0x40)
	data = append(data, make([]byte, 16)...) // AAGUID
	data = binary.BigEndian.AppendUint16(data, uint16(len(a.ID)))
	data = append(append(data, a.ID...), a.coseKey()...)
	return a.clientData("webauthn.create", challenge), cborMap(cborText("fmt"), cborText("none"), cborText("attStmt"), cborMap(), cborText("authData"), cborBytes(data))
}

// Get answers navigator.credentials.get().
func (a *Authenticator) Get(challenge string) (clientDataJSON, authenticatorData, signature []byte) {
	clientDataJSON, authenticatorData = a.clientData("webauthn.get", challenge), a.authData(a.Flags)
	hash := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authenticatorData...), hash[:]...)
	var opts crypto.SignerOpts = crypto.Hash(0)
	if a.Alg != EdDSA {
		digest := sha256.Sum256(signed)
		signed, opts = digest[:], crypto.SHA256
	}
	signature, _ = a.key.Sign(rand.Reader, signed, opts)
	return
}
