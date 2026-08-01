package vault

import (
	"bytes"
	"errors"
	"testing"
)

func TestPassphraseRoundTrip(t *testing.T) {
	plain := []byte(`{"accounts":[{"token":"secret"}]}`)
	aad := []byte("omnidrive/test")

	sealed, err := Seal("correct horse battery", plain, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("plaintext leaked into the sealed payload")
	}

	got, err := Open("correct horse battery", sealed, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", got, plain)
	}
}

func TestWrongPassphraseFails(t *testing.T) {
	sealed, err := Seal("right passphrase", []byte("data"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("wrong passphrase", sealed, nil); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("want ErrBadPassphrase, got %v", err)
	}
}

// Every byte of the payload is authenticated, so a single flipped bit must be
// rejected rather than silently decrypted into garbage.
func TestTamperIsDetected(t *testing.T) {
	sealed, err := Seal("passphrase here", bytes.Repeat([]byte("a"), 256), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int{0, 10, len(sealed) / 2, len(sealed) - 1} {
		corrupt := append([]byte(nil), sealed...)
		corrupt[pos] ^= 0x01
		if _, err := Open("passphrase here", corrupt, nil); err == nil {
			t.Fatalf("corruption at byte %d was not detected", pos)
		}
	}
}

func TestAADMismatchFails(t *testing.T) {
	sealed, err := Seal("passphrase here", []byte("data"), []byte("context-a"))
	if err != nil {
		t.Fatal(err)
	}
	// A bundle must not be openable as if it were device state, even with the
	// right passphrase.
	if _, err := Open("passphrase here", sealed, []byte("context-b")); err == nil {
		t.Fatal("mismatched additional data was accepted")
	}
}

func TestRawKeyRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealWithKey(key, []byte("device state"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenWithKey(key, sealed, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "device state" {
		t.Fatalf("got %q", got)
	}

	other, _ := NewKey()
	if _, err := OpenWithKey(other, sealed, []byte("aad")); err == nil {
		t.Fatal("a different key opened the payload")
	}
}

// The two modes must not be interchangeable: opening a key-sealed payload with
// a passphrase should fail loudly rather than produce a confusing MAC error.
func TestModesAreDistinct(t *testing.T) {
	key, _ := NewKey()
	keySealed, _ := SealWithKey(key, []byte("x"), nil)
	if _, err := Open("some passphrase", keySealed, nil); err == nil {
		t.Fatal("passphrase Open accepted a key-sealed payload")
	}
	if IsPassphraseSealed(keySealed) {
		t.Fatal("key-sealed payload reported as passphrase-sealed")
	}

	passSealed, _ := Seal("some passphrase", []byte("x"), nil)
	if _, err := OpenWithKey(key, passSealed, nil); err == nil {
		t.Fatal("key Open accepted a passphrase-sealed payload")
	}
	if !IsPassphraseSealed(passSealed) {
		t.Fatal("passphrase-sealed payload not detected")
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{nil, []byte("short"), []byte("NOTAVAULTHEADERAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")} {
		if _, err := Open("passphrase here", bad, nil); err == nil {
			t.Fatalf("garbage input %q was accepted", bad)
		}
	}
}
