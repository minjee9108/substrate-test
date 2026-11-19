package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
)

// pinStore remembers the SSH host key fingerprint of each actor ("<atespace>/<actor>")
// the first time the gateway connects to it, and refuses a different key later.
//
// The path to an actor is already authenticated hop by hop (gateway certificate
// to the router, router to the worker), so this does not defend that path; it
// catches an actor that has been replaced or tampered with, and keeps the
// guarantee that every actor has its own key.
//
// PROTOTYPE: a JSON file. A deployed gateway needs shared storage (a ConfigMap
// or database) so replicas agree, and should pin to the actor's UID so a
// deleted-and-recreated actor of the same name is not mistaken for tampering.
type pinStore struct {
	mu   sync.Mutex
	path string // "" keeps pins in memory only
	pins map[string]string
}

func loadPins(path string) (*pinStore, error) {
	p := &pinStore{path: path, pins: map[string]string{}}
	if path == "" {
		return p, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	// A corrupt file is an error, never an empty store: resetting would
	// silently re-trust every actor.
	if err := json.Unmarshal(b, &p.pins); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// verify pins key for actorKey on first use and rejects a different key after.
func (p *pinStore) verify(actorKey string, key ssh.PublicKey) (firstUse bool, err error) {
	fp := ssh.FingerprintSHA256(key)
	p.mu.Lock()
	defer p.mu.Unlock()
	if pinned, ok := p.pins[actorKey]; ok {
		if pinned != fp {
			return false, fmt.Errorf("host key of %s changed: pinned %s, now %s (if the actor was deliberately recreated, remove its entry from %s)",
				actorKey, pinned, fp, p.pathOrMemory())
		}
		return false, nil
	}
	p.pins[actorKey] = fp
	if err := p.save(); err != nil {
		delete(p.pins, actorKey)
		return false, fmt.Errorf("recording host key of %s: %w", actorKey, err)
	}
	return true, nil
}

func (p *pinStore) pathOrMemory() string {
	if p.path == "" {
		return "the in-memory store"
	}
	return p.path
}

func (p *pinStore) save() error {
	if p.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(p.pins, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".pins-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.path)
}
