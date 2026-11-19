package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// A test token is a compact JWT signed with EdDSA. It stands in for the
// Entra ID token until OIDC login is wired in; the HTTP gateway only needs
// the subject and expiry from either.

type tokenClaims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

var b64 = base64.RawURLEncoding

const tokenHeader = `{"alg":"EdDSA","typ":"JWT"}`

func signToken(priv ed25519.PrivateKey, sub string, ttl time.Duration, now time.Time) (string, error) {
	c, err := json.Marshal(tokenClaims{Sub: sub, Exp: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString([]byte(tokenHeader)) + "." + b64.EncodeToString(c)
	return signing + "." + b64.EncodeToString(ed25519.Sign(priv, []byte(signing))), nil
}

// verifyToken checks the signature against any of keys and the expiry.
func verifyToken(keys []ed25519.PublicKey, tok string, now time.Time) (tokenClaims, error) {
	var zero tokenClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return zero, errors.New("malformed token")
	}
	hdr, err := b64.DecodeString(parts[0])
	if err != nil || string(hdr) != tokenHeader {
		return zero, errors.New("unsupported token header")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return zero, errors.New("malformed signature")
	}
	signing := []byte(parts[0] + "." + parts[1])
	ok := false
	for _, k := range keys {
		if ed25519.Verify(k, signing, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return zero, errors.New("bad signature")
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return zero, errors.New("malformed claims")
	}
	var c tokenClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.Sub == "" || c.Exp == 0 {
		return zero, errors.New("malformed claims")
	}
	if !now.Before(time.Unix(c.Exp, 0)) {
		return zero, errors.New("token expired")
	}
	return c, nil
}

// loadTokenKeys reads base64 Ed25519 public keys, one per line.
func loadTokenKeys(path string) ([]ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s: not a base64 Ed25519 public key", path)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s holds no keys", path)
	}
	return keys, nil
}

// issueTokenMain implements `ssh-gateway issue-token`. It creates the key
// pair on first use (<key> holds the private key, <key>.pub the public one).
func issueTokenMain(args []string) {
	fs := flag.NewFlagSet("issue-token", flag.ExitOnError)
	keyPath := fs.String("key", "", "private key file (created with <key>.pub if missing)")
	sub := fs.String("sub", "", "principal the token acts as")
	ttl := fs.Duration("ttl", 8*time.Hour, "token lifetime")
	fs.Parse(args)
	if *keyPath == "" || *sub == "" {
		log.Fatal("issue-token: -key and -sub are required")
	}
	var priv ed25519.PrivateKey
	if b, err := os.ReadFile(*keyPath); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			log.Fatalf("%s: not a base64 Ed25519 private key", *keyPath)
		}
		priv = ed25519.PrivateKey(raw)
	} else if os.IsNotExist(err) {
		pub, p, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			log.Fatal(err)
		}
		priv = p
		if err := os.WriteFile(*keyPath, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*keyPath+".pub", []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
	} else {
		log.Fatal(err)
	}
	tok, err := signToken(priv, *sub, *ttl, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(tok)
}
