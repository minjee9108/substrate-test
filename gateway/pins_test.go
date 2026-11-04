package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinsFirstUseThenMatch(t *testing.T) {
	p, _ := loadPins("")
	k1 := newSigner(t).PublicKey()
	if first, err := p.verify("ns/a", k1); !first || err != nil {
		t.Fatalf("first use should pin: first=%v err=%v", first, err)
	}
	if first, err := p.verify("ns/a", k1); first || err != nil {
		t.Fatalf("same key should pass without re-pinning: first=%v err=%v", first, err)
	}
}

func TestPinsRejectChangedKey(t *testing.T) {
	p, _ := loadPins("")
	if _, err := p.verify("ns/a", newSigner(t).PublicKey()); err != nil {
		t.Fatal(err)
	}
	_, err := p.verify("ns/a", newSigner(t).PublicKey())
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a different key for the same actor must be refused, got %v", err)
	}
}

func TestPinsAreIndependentPerActor(t *testing.T) {
	p, _ := loadPins("")
	if _, err := p.verify("ns/a", newSigner(t).PublicKey()); err != nil {
		t.Fatal(err)
	}
	if first, err := p.verify("ns/b", newSigner(t).PublicKey()); !first || err != nil {
		t.Fatalf("another actor gets its own pin: first=%v err=%v", first, err)
	}
}

func TestPinsPersistAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins.json")
	k := newSigner(t).PublicKey()
	p, _ := loadPins(path)
	if _, err := p.verify("ns/a", k); err != nil {
		t.Fatal(err)
	}
	p2, err := loadPins(path)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := p2.verify("ns/a", k); first || err != nil {
		t.Fatalf("pin should survive a restart: first=%v err=%v", first, err)
	}
	if _, err := p2.verify("ns/a", newSigner(t).PublicKey()); err == nil {
		t.Fatal("a changed key must still be refused after a restart")
	}
}

func TestPinsCorruptFileIsAnErrorNotAReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins.json")
	os.WriteFile(path, []byte("{truncated"), 0o600)
	if _, err := loadPins(path); err == nil {
		t.Fatal("a corrupt pin file must fail closed, not start empty")
	}
}
