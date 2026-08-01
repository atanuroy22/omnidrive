// Package portable moves a complete OmniDrive setup — every connected
// account, its OAuth tokens, the app registrations and your settings — to
// another device, so a new phone is usable in under a minute instead of being
// re-authorised drive by drive.
//
// Three transports share one encrypted container format:
//
//	file   — export to a .omnibundle you copy however you like
//	pair   — one device serves the bundle to another over the local network
//	cloud  — the bundle is stored inside a drive you have already connected,
//	         so a new device only needs one account to recover the rest
package portable

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"omnidrive/internal/store"
	"omnidrive/internal/vault"
)

// BundleName is the conventional filename used by the file and cloud transports.
const BundleName = "omnidrive-config.omnibundle"

const (
	bundleVersion = 1
	bundleAAD     = "omnidrive/bundle/v1"
	maxBundle     = 32 << 20 // a sane ceiling; real bundles are a few KiB
)

// Bundle is the plaintext document that gets sealed.
type Bundle struct {
	Version   int         `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	Device    string      `json:"device"`
	Platform  string      `json:"platform"`
	State     store.State `json:"state"`
}

// Summary describes a bundle without exposing its secrets, so the UI can show
// the user what they are about to import.
type Summary struct {
	CreatedAt time.Time `json:"createdAt"`
	Device    string    `json:"device"`
	Platform  string    `json:"platform"`
	Accounts  []string  `json:"accounts"`
}

// ErrPassphraseRequired means a bundle was protected with a passphrase the
// caller did not supply, as opposed to being corrupt.
var ErrPassphraseRequired = errors.New("this bundle is passphrase-protected")

// builtinKey seals bundles when no passphrase is given, so exporting and
// restoring are one tap.
//
// This is obfuscation, not protection: the key is in every copy of the binary,
// so anyone with the file and the app can open it. It keeps credentials from
// sitting in plain sight, nothing more — an unprotected bundle should be
// treated as sensitively as the passwords inside it. Supply a passphrase and
// the file becomes genuinely protected instead.
const builtinKey = "omnidrive/unprotected-bundle/v1"

func sealingKey(passphrase string) string {
	if passphrase == "" {
		return builtinKey
	}
	return passphrase
}

// Export seals the store's full state.
//
// An empty passphrase produces an unprotected bundle that restores without one.
func Export(st *store.Store, passphrase string) ([]byte, error) {
	if passphrase != "" && len(passphrase) < 8 {
		return nil, errors.New("a passphrase must be at least 8 characters; leave it blank for none")
	}
	b := Bundle{
		Version:   bundleVersion,
		CreatedAt: time.Now().UTC(),
		Device:    deviceName(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		State:     st.Snapshot(),
	}
	plain, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	// Token blobs compress well; a bundle with a dozen accounts drops from
	// ~30 KiB to ~6 KiB, which matters for the QR-free pairing flow.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return vault.Seal(sealingKey(passphrase), gz.Bytes(), []byte(bundleAAD))
}

// Decode opens a sealed bundle.
func Decode(blob []byte, passphrase string) (*Bundle, error) {
	if len(blob) > maxBundle {
		return nil, fmt.Errorf("bundle too large (%d bytes)", len(blob))
	}
	gzBytes, err := vault.Open(sealingKey(passphrase), blob, []byte(bundleAAD))
	if err != nil {
		// With no passphrase supplied, the built-in key was tried. Failing
		// that, the bundle is protected — say so, rather than reporting it as
		// corrupt.
		if passphrase == "" {
			return nil, ErrPassphraseRequired
		}
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		return nil, fmt.Errorf("bundle is not valid gzip: %w", err)
	}
	defer zr.Close()

	plain, err := io.ReadAll(io.LimitReader(zr, maxBundle))
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, fmt.Errorf("bundle is not valid JSON: %w", err)
	}
	if b.Version > bundleVersion {
		return nil, fmt.Errorf("bundle was written by a newer OmniDrive (format v%d); upgrade first", b.Version)
	}
	return &b, nil
}

// Summarize returns a safe description of a decoded bundle.
func (b *Bundle) Summarize() Summary {
	s := Summary{CreatedAt: b.CreatedAt, Device: b.Device, Platform: b.Platform}
	for _, a := range b.State.Accounts {
		s.Accounts = append(s.Accounts, fmt.Sprintf("%s (%s)", a.Label, a.Kind))
	}
	return s
}

// Import decrypts a bundle and merges it into the store. When replace is true
// the local account list is discarded first; otherwise accounts are merged by
// ID, so importing the same bundle twice changes nothing.
func Import(st *store.Store, blob []byte, passphrase string, replace bool) (Summary, int, int, error) {
	b, err := Decode(blob, passphrase)
	if err != nil {
		return Summary{}, 0, 0, err
	}
	added, updated, err := st.Restore(b.State, replace)
	if err != nil {
		return Summary{}, 0, 0, err
	}
	return b.Summarize(), added, updated, nil
}

func deviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-device"
}
