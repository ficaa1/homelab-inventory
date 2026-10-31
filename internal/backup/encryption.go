package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// Envelope v1 uses PBKDF2-HMAC-SHA256 (600,000 iterations), AES-256-CTR,
// and encrypt-then-HMAC-SHA256 over the complete header and ciphertext.
// Authentication is verified on a private disk spool before plaintext is read.
const envelopeMagic = "HLBACKUP\x01"
const iterations = 600000
const headerSize = len(envelopeMagic) + 4 + 32 + aes.BlockSize

var ErrPassphrase = errors.New("backup authentication failed (wrong passphrase or damaged file)")

func keys(pass string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, iterations, 64)
}
func Encrypt(ctx context.Context, out io.Writer, pass string, write func(io.Writer) error) error {
	if len(pass) < 12 {
		return errors.New("use a passphrase of at least 12 characters")
	}
	header := make([]byte, headerSize)
	copy(header, envelopeMagic)
	binary.BigEndian.PutUint32(header[len(envelopeMagic):], iterations)
	if _, e := rand.Read(header[len(envelopeMagic)+4:]); e != nil {
		return e
	}
	salt := header[len(envelopeMagic)+4 : len(envelopeMagic)+4+32]
	key, e := keys(pass, salt)
	if e != nil {
		return e
	}
	block, e := aes.NewCipher(key[:32])
	if e != nil {
		return e
	}
	mac := hmac.New(sha256.New, key[32:])
	if _, e = io.MultiWriter(out, mac).Write(header); e != nil {
		return e
	}
	stream := cipher.NewCTR(block, header[headerSize-aes.BlockSize:])
	writer := cipher.StreamWriter{S: stream, W: io.MultiWriter(out, mac)}
	if e = write(&writer); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	_, e = out.Write(mac.Sum(nil))
	return e
}

// Decrypt spools bounded encrypted bytes in parent, verifies the entire envelope,
// and returns a plaintext reader plus cleanup. Neither passphrase nor key persists.
func Decrypt(ctx context.Context, in io.Reader, parent, pass string, max int64) (io.Reader, func(), error) {
	f, e := os.CreateTemp(parent, ".backup-cipher-")
	if e != nil {
		return nil, nil, e
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	fail := func(e error) (io.Reader, func(), error) { cleanup(); return nil, nil, e }
	n, e := copyContext(ctx, f, io.LimitReader(in, max+1))
	if e != nil {
		return fail(e)
	}
	if n > max || n < int64(headerSize+sha256.Size) {
		return fail(ErrPassphrase)
	}
	if _, e = f.Seek(0, 0); e != nil {
		return fail(e)
	}
	header := make([]byte, headerSize)
	if _, e = io.ReadFull(f, header); e != nil {
		return fail(ErrPassphrase)
	}
	if string(header[:len(envelopeMagic)]) != envelopeMagic || binary.BigEndian.Uint32(header[len(envelopeMagic):]) != iterations {
		return fail(ErrPassphrase)
	}
	key, e := keys(pass, header[len(envelopeMagic)+4:len(envelopeMagic)+4+32])
	if e != nil {
		return fail(e)
	}
	mac := hmac.New(sha256.New, key[32:])
	mac.Write(header)
	cipherBytes := n - int64(headerSize+sha256.Size)
	if _, e = copyContext(ctx, mac, io.LimitReader(f, cipherBytes)); e != nil {
		return fail(e)
	}
	tag := make([]byte, sha256.Size)
	if _, e = io.ReadFull(f, tag); e != nil || !hmac.Equal(mac.Sum(nil), tag) {
		return fail(ErrPassphrase)
	}
	if _, e = f.Seek(int64(headerSize), 0); e != nil {
		return fail(e)
	}
	block, e := aes.NewCipher(key[:32])
	if e != nil {
		return fail(e)
	}
	return cipher.StreamReader{S: cipher.NewCTR(block, header[headerSize-aes.BlockSize:]), R: io.LimitReader(f, cipherBytes)}, cleanup, nil
}
