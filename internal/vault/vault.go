// Package vault implements authenticated encryption for everything at rest:
// the on-device state file and the portable backup bundles.
//
// Only the standard library is used — PBKDF2-SHA256 for password stretching
// and AES-256-GCM for the sealed payload — so the binary stays small and
// cross-compiles without CGO.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	magic       = "OMNIVLT1"
	keyLen      = 32
	saltLen     = 16
	nonceLen    = 12
	headerLen   = len(magic) + 1 + saltLen + 4 + nonceLen
	defaultIter = 210_000 // OWASP 2023 floor for PBKDF2-SHA256
)

// ErrBadPassphrase means the payload failed authentication: a wrong password,
// or a corrupted/tampered file. The two are indistinguishable by design.
var ErrBadPassphrase = errors.New("wrong passphrase or corrupted data")

// NewKey returns a fresh random data key, used for local state encryption
// where no human passphrase is involved.
func NewKey() ([]byte, error) {
	k := make([]byte, keyLen)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

// SealWithKey encrypts plaintext under a raw 32-byte key.
func SealWithKey(key, plaintext, aad []byte) ([]byte, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", keyLen, len(key))
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Raw-key payloads carry a zero salt and zero iteration count so the
	// container format stays identical to the passphrase form.
	out := make([]byte, 0, headerLen+len(plaintext)+gcm.Overhead())
	out = append(out, magic...)
	out = append(out, 0x01) // 0x01 = raw key
	out = append(out, make([]byte, saltLen)...)
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), nil
}

// OpenWithKey decrypts a payload produced by SealWithKey.
func OpenWithKey(key, blob, aad []byte) ([]byte, error) {
	mode, _, _, nonce, ct, err := parse(blob)
	if err != nil {
		return nil, err
	}
	if mode != 0x01 {
		return nil, errors.New("vault: payload is passphrase-protected, not key-protected")
	}
	return open(key, nonce, ct, aad)
}

// Seal encrypts plaintext under a human passphrase.
func Seal(passphrase string, plaintext, aad []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	key, err := derive(passphrase, salt, defaultIter)
	if err != nil {
		return nil, err
	}
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, headerLen+len(plaintext)+gcm.Overhead())
	out = append(out, magic...)
	out = append(out, 0x02) // 0x02 = PBKDF2 passphrase
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, defaultIter)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a passphrase-sealed payload.
func Open(passphrase string, blob, aad []byte) ([]byte, error) {
	mode, salt, iter, nonce, ct, err := parse(blob)
	if err != nil {
		return nil, err
	}
	if mode != 0x02 {
		return nil, errors.New("vault: payload is key-protected, not passphrase-protected")
	}
	if iter == 0 || iter > 10_000_000 {
		return nil, errors.New("vault: implausible iteration count")
	}
	key, err := derive(passphrase, salt, int(iter))
	if err != nil {
		return nil, err
	}
	return open(key, nonce, ct, aad)
}

// IsPassphraseSealed reports whether blob needs a password to open. Callers use
// it to decide whether to prompt before attempting a decrypt.
func IsPassphraseSealed(blob []byte) bool {
	mode, _, _, _, _, err := parse(blob)
	return err == nil && mode == 0x02
}

func parse(blob []byte) (mode byte, salt []byte, iter uint32, nonce, ct []byte, err error) {
	if len(blob) < headerLen {
		return 0, nil, 0, nil, nil, errors.New("vault: payload too short")
	}
	if string(blob[:len(magic)]) != magic {
		return 0, nil, 0, nil, nil, errors.New("vault: not an OmniDrive vault payload")
	}
	p := len(magic)
	mode = blob[p]
	p++
	salt = blob[p : p+saltLen]
	p += saltLen
	iter = binary.BigEndian.Uint32(blob[p : p+4])
	p += 4
	nonce = blob[p : p+nonceLen]
	p += nonceLen
	ct = blob[p:]
	return mode, salt, iter, nonce, ct, nil
}

func open(key, nonce, ct, aad []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return pt, nil
}

func derive(passphrase string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, passphrase, salt, iter, keyLen)
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
