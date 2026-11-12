package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

// This file is the WebAuthn relying party, on the standard library alone. It
// checks what a passkey sign-in depends on: the challenge, the page origin, the
// relying-party ID hash, user presence and the signature. Attestation statements
// are not verified (the page asks for "none"): a passkey is trusted because an
// authorized admin registered it, not because of who made the authenticator.

// COSE algorithm identifiers the page offers, in order of preference.
const (
	algES256 = -7
	algEdDSA = -8
	algRS256 = -257
)

// PasskeyAlgorithms is the pubKeyCredParams list for registration.
var PasskeyAlgorithms = []int{algES256, algEdDSA, algRS256}

const (
	flagUserPresent  = 0x01
	flagAttestedData = 0x40
)

// MaxPasskeys bounds the stored list.
const MaxPasskeys = 20

// Passkey is one registered credential. Origin is the page it was registered
// on; sign-in is accepted only from that origin, and its host is the
// relying-party ID. PublicKey is base64 PKIX DER.
type Passkey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Origin    string     `json:"origin"`
	Algorithm int        `json:"alg"`
	PublicKey string     `json:"public_key"`
	SignCount uint32     `json:"sign_count"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`
}

// Passkey finds a registered credential by its base64url ID.
func (c Config) Passkey(id string) (Passkey, bool) {
	for _, p := range c.Passkeys {
		if id != "" && p.ID == id {
			return p, true
		}
	}
	return Passkey{}, false
}

// PasskeyRPID returns the relying-party ID for a page origin, which is its host
// name. Browsers offer WebAuthn only over HTTPS or on http://localhost, and
// never for an IP address, so other origins are refused here too.
func PasskeyRPID(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || origin != u.Scheme+"://"+u.Host || origin != strings.ToLower(origin) {
		return "", false
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, "[]%") {
		return "", false
	}
	if u.Scheme == "https" || u.Scheme == "http" && host == "localhost" {
		return host, true
	}
	return "", false
}

// ValidPasskeyName accepts a short single-line label.
func ValidPasskeyName(name string) bool {
	if name == "" || len(name) > 64 || name != strings.TrimSpace(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func parseClientData(raw []byte, kind, origin string) (clientData, error) {
	var cd clientData
	if json.Unmarshal(raw, &cd) != nil || cd.Type != kind || cd.Challenge == "" {
		return cd, errors.New("client data")
	}
	if cd.CrossOrigin || cd.Origin != origin {
		return cd, errors.New("origin")
	}
	return cd, nil
}

// authenticatorData is the fixed head of the structure an authenticator signs.
type authenticatorData struct {
	flags     byte
	signCount uint32
	rest      []byte
}

func parseAuthenticatorData(b []byte, rpID string) (authenticatorData, error) {
	if len(b) < 37 {
		return authenticatorData{}, errors.New("authenticator data")
	}
	want := sha256.Sum256([]byte(rpID))
	if !bytes.Equal(b[:32], want[:]) {
		return authenticatorData{}, errors.New("relying party")
	}
	d := authenticatorData{flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37]), rest: b[37:]}
	if d.flags&flagUserPresent == 0 {
		return authenticatorData{}, errors.New("user presence")
	}
	return d, nil
}

// parseRegistration checks a navigator.credentials.create() answer made on
// origin and returns the new credential, without a name. The challenge in the
// client data is left for the caller to redeem.
func parseRegistration(clientDataJSON, attestationObject []byte, origin string, now time.Time) (Passkey, string, error) {
	rpID, ok := PasskeyRPID(origin)
	if !ok {
		return Passkey{}, "", errors.New("origin")
	}
	cd, err := parseClientData(clientDataJSON, "webauthn.create", origin)
	if err != nil {
		return Passkey{}, "", err
	}
	item, _, err := cborItem(attestationObject, 0)
	if err != nil {
		return Passkey{}, "", errors.New("attestation")
	}
	att, _ := item.(map[any]any)
	raw, _ := att["authData"].([]byte)
	d, err := parseAuthenticatorData(raw, rpID)
	if err != nil {
		return Passkey{}, "", err
	}
	// Attested credential data: AAGUID(16), ID length(2), ID, COSE public key.
	if d.flags&flagAttestedData == 0 || len(d.rest) < 18 {
		return Passkey{}, "", errors.New("credential data")
	}
	n := int(binary.BigEndian.Uint16(d.rest[16:18]))
	if n == 0 || n > 1023 || len(d.rest) < 18+n {
		return Passkey{}, "", errors.New("credential data")
	}
	id := d.rest[18 : 18+n]
	item, _, err = cborItem(d.rest[18+n:], 0)
	if err != nil {
		return Passkey{}, "", errors.New("public key")
	}
	key, _ := item.(map[any]any)
	alg, pub, err := coseKey(key)
	if err != nil {
		return Passkey{}, "", err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return Passkey{}, "", errors.New("public key")
	}
	return Passkey{ID: base64.RawURLEncoding.EncodeToString(id), Origin: origin, Algorithm: alg,
		PublicKey: base64.StdEncoding.EncodeToString(der), SignCount: d.signCount, CreatedAt: now.UTC().Truncate(time.Second)}, cd.Challenge, nil
}

// coseKey reads a COSE_Key map into a Go public key for one of PasskeyAlgorithms.
func coseKey(m map[any]any) (int, crypto.PublicKey, error) {
	bad := errors.New("public key")
	num := func(k int64) int64 { v, _ := m[k].(int64); return v }
	raw := func(k int64) []byte { v, _ := m[k].([]byte); return v }
	switch kty, alg := num(1), num(3); {
	case kty == 2 && alg == algES256:
		x, y := raw(-2), raw(-3)
		if num(-1) != 1 || len(x) != 32 || len(y) != 32 {
			return 0, nil, bad
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return 0, nil, bad
		}
		return algES256, pub, nil
	case kty == 1 && alg == algEdDSA:
		x := raw(-2)
		if num(-1) != 6 || len(x) != ed25519.PublicKeySize {
			return 0, nil, bad
		}
		return algEdDSA, ed25519.PublicKey(x), nil
	case kty == 3 && alg == algRS256:
		n, e := new(big.Int).SetBytes(raw(-1)), new(big.Int).SetBytes(raw(-2))
		if n.BitLen() < 2048 || n.BitLen() > 8192 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Int64()%2 == 0 {
			return 0, nil, bad
		}
		return algRS256, &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	}
	return 0, nil, bad
}

// verifies checks an assertion signature over authenticatorData and the
// client data hash with the stored public key.
func (p Passkey) verifies(authData, clientDataJSON, signature []byte) bool {
	der, err := base64.StdEncoding.DecodeString(p.PublicKey)
	if err != nil {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authData...), hash[:]...)
	digest := sha256.Sum256(signed)
	switch key := pub.(type) {
	case *ecdsa.PublicKey:
		return p.Algorithm == algES256 && key.Curve == elliptic.P256() && ecdsa.VerifyASN1(key, digest[:], signature)
	case ed25519.PublicKey:
		return p.Algorithm == algEdDSA && ed25519.Verify(key, signed, signature)
	case *rsa.PublicKey:
		return p.Algorithm == algRS256 && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
	}
	return false
}

// verifyAssertion checks a navigator.credentials.get() answer against p and
// returns the authenticator's signature counter. The challenge in the client
// data is left for the caller to redeem.
func (p Passkey) verifyAssertion(clientDataJSON, authData, signature []byte) (uint32, string, error) {
	rpID, ok := PasskeyRPID(p.Origin)
	if !ok {
		return 0, "", errors.New("origin")
	}
	cd, err := parseClientData(clientDataJSON, "webauthn.get", p.Origin)
	if err != nil {
		return 0, "", err
	}
	d, err := parseAuthenticatorData(authData, rpID)
	if err != nil {
		return 0, "", err
	}
	if !p.verifies(authData, clientDataJSON, signature) {
		return 0, "", errors.New("signature")
	}
	// A counter that fails to advance means the credential was copied. Zero on
	// both sides is an authenticator that keeps no counter, such as a synced passkey.
	if (d.signCount != 0 || p.SignCount != 0) && d.signCount <= p.SignCount {
		return 0, "", errors.New("counter")
	}
	return d.signCount, cd.Challenge, nil
}
