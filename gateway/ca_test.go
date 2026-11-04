package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthenticateCert(t *testing.T) {
	ca, otherCA, user := newSigner(t), newSigner(t), newSigner(t)
	now := time.Now()
	checker := newCertChecker([]ssh.PublicKey{ca.PublicKey()})
	const maxLife = 24 * time.Hour

	issue := func(signer ssh.Signer, principal string, ttl time.Duration, at time.Time) *ssh.Certificate {
		c, err := issueCert(signer, user.PublicKey(), principal, ttl, at)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	mutate := func(c *ssh.Certificate, f func(*ssh.Certificate)) *ssh.Certificate {
		f(c)
		if err := c.SignCert(rand.Reader, ca); err != nil {
			t.Fatal(err)
		}
		return c
	}

	tests := []struct {
		name    string
		cert    *ssh.Certificate
		wantErr string // substring; "" means success
	}{
		{"valid", issue(ca, "alice", 8*time.Hour, now), ""},
		{"principal with colons", issue(ca, "system:serviceaccount:team-a:alice", time.Hour, now), ""},
		{"signed by an untrusted CA", issue(otherCA, "alice", time.Hour, now), "authority"},
		{"expired", issue(ca, "alice", time.Hour, now.Add(-3*time.Hour)), "expired"},
		{"not yet valid", issue(ca, "alice", time.Hour, now.Add(3*time.Hour)), "not yet valid"},
		{"lifetime above the cap", issue(ca, "alice", 48*time.Hour, now), "exceeds"},
		{"host certificate", mutate(issue(ca, "alice", time.Hour, now), func(c *ssh.Certificate) { c.CertType = ssh.HostCert }), "not a user certificate"},
		{"no principal", mutate(issue(ca, "alice", time.Hour, now), func(c *ssh.Certificate) { c.ValidPrincipals = nil }), "exactly one principal"},
		{"two principals", mutate(issue(ca, "alice", time.Hour, now), func(c *ssh.Certificate) { c.ValidPrincipals = []string{"alice", "bob"} }), "exactly one principal"},
		{"no expiry", mutate(issue(ca, "alice", time.Hour, now), func(c *ssh.Certificate) { c.ValidBefore = ssh.CertTimeInfinity }), "no expiry"},
		{"unknown critical option", mutate(issue(ca, "alice", time.Hour, now), func(c *ssh.Certificate) {
			c.CriticalOptions = map[string]string{"made-up-option": ""}
		}), "critical option"},
		{"tampered after signing", func() *ssh.Certificate {
			c := issue(ca, "alice", time.Hour, now)
			c.ValidPrincipals = []string{"carol"} // signature no longer matches
			return c
		}(), "signature"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			principal, expiry, err := authenticateCert(checker, tt.cert, maxLife)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if principal != tt.cert.ValidPrincipals[0] || !expiry.After(now) {
					t.Fatalf("got principal %q expiry %v", principal, expiry)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got principal %q", tt.wantErr, principal)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCARotationTrustsEitherKey(t *testing.T) {
	oldCA, newCA, user := newSigner(t), newSigner(t), newSigner(t)
	checker := newCertChecker([]ssh.PublicKey{oldCA.PublicKey(), newCA.PublicKey()})
	for _, ca := range []ssh.Signer{oldCA, newCA} {
		c, _ := issueCert(ca, user.PublicKey(), "alice", time.Hour, time.Now())
		if _, _, err := authenticateCert(checker, c, 24*time.Hour); err != nil {
			t.Fatalf("both CAs should be trusted during rotation: %v", err)
		}
	}
}
