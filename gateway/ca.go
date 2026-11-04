package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// A user certificate is the gateway's proof of identity. Its single valid
// principal is the Substrate principal (for Entra: the token's `sub`, which is
// what ate-api derives, so access policies bind the same string). The SSH
// username stays the target (<actor>.<atespace>), so the certificate is never
// matched against the username.

// loadCAs reads trusted CA public keys, one per line (authorized_keys format).
// Several lines allow CA rotation.
func loadCAs(path string) ([]ssh.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cas []ssh.PublicKey
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("ca file %s: %w", path, err)
		}
		cas = append(cas, pk)
	}
	if len(cas) == 0 {
		return nil, fmt.Errorf("ca file %s: no keys", path)
	}
	return cas, nil
}

func newCertChecker(cas []ssh.PublicKey) *ssh.CertChecker {
	return &ssh.CertChecker{
		IsUserAuthority: func(auth ssh.PublicKey) bool {
			for _, ca := range cas {
				if bytes.Equal(auth.Marshal(), ca.Marshal()) {
					return true
				}
			}
			return false
		},
	}
}

// authenticateCert validates cert and returns the principal it names and when
// it expires. It requires a trusted signing authority and, via CheckCert, a
// valid signature and validity window, plus: a user certificate, exactly one principal,
// a finite validity, and a lifetime within maxLifetime, so a mis-issued
// long-lived certificate cannot grant access for long.
func authenticateCert(checker *ssh.CertChecker, cert *ssh.Certificate, maxLifetime time.Duration) (string, time.Time, error) {
	if cert.CertType != ssh.UserCert {
		return "", time.Time{}, fmt.Errorf("not a user certificate")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] == "" {
		return "", time.Time{}, fmt.Errorf("certificate must name exactly one principal")
	}
	principal := cert.ValidPrincipals[0]
	// CheckCert does NOT check who signed the certificate (CertChecker.Authenticate
	// does, and we cannot use it because the username is the target, not the
	// principal). Without this line any CA's certificate would be accepted.
	if !checker.IsUserAuthority(cert.SignatureKey) {
		return "", time.Time{}, fmt.Errorf("certificate signed by an unrecognized authority")
	}
	if err := checker.CheckCert(principal, cert); err != nil {
		return "", time.Time{}, err
	}
	if cert.ValidBefore == ssh.CertTimeInfinity {
		return "", time.Time{}, fmt.Errorf("certificate has no expiry")
	}
	notAfter := time.Unix(int64(cert.ValidBefore), 0)
	if life := notAfter.Sub(time.Unix(int64(cert.ValidAfter), 0)); life > maxLifetime {
		return "", time.Time{}, fmt.Errorf("certificate lifetime %s exceeds the allowed %s", life, maxLifetime)
	}
	return principal, notAfter, nil
}

// issueCert signs a user certificate. PROTOTYPE: the real issuer first verifies
// the user's Entra token (signature, tenant, audience, expiry) and takes
// principal from its `sub`; it must never trust a principal the caller names.
func issueCert(ca ssh.Signer, userPub ssh.PublicKey, principal string, ttl time.Duration, now time.Time) (*ssh.Certificate, error) {
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return nil, err
	}
	cert := &ssh.Certificate{
		Key:             userPub,
		Serial:          binary.BigEndian.Uint64(serial[:]),
		CertType:        ssh.UserCert,
		KeyId:           principal + " (test CA)",
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(now.Add(-time.Minute).Unix()), // clock-skew allowance
		ValidBefore:     uint64(now.Add(ttl).Unix()),
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		return nil, err
	}
	return cert, nil
}

// issueMain implements `ssh-gateway issue`, a stand-in for the login service.
func issueMain(args []string) {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	caPath := fs.String("ca", "", "CA private key")
	pubPath := fs.String("pub", "", "user public key to certify")
	principal := fs.String("principal", "", "principal to put in the certificate")
	ttl := fs.Duration("ttl", 8*time.Hour, "certificate lifetime")
	_ = fs.Parse(args)
	if *caPath == "" || *pubPath == "" || *principal == "" {
		fs.Usage()
		os.Exit(2)
	}
	ca, err := readSigner(*caPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ca key:", err)
		os.Exit(1)
	}
	pb, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pub key:", err)
		os.Exit(1)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pub key:", err)
		os.Exit(1)
	}
	cert, err := issueCert(ca, pub, *principal, *ttl, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue:", err)
		os.Exit(1)
	}
	os.Stdout.Write(ssh.MarshalAuthorizedKey(cert))
}
