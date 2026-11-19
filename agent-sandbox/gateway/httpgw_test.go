package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseHost(t *testing.T) {
	for _, c := range []struct {
		host, actor, space string
		bad                bool
	}{
		{"desk-4--ate-spike-ssh.localhost:8080", "desk-4", "ate-spike-ssh", false},
		{"Desk-4--Team.localhost", "desk-4", "team", false},
		{"desk-4.localhost", "", "", true},
		{"desk-4--team.example.com", "", "", true},
		{"--team.localhost", "", "", true},
		{"a--b--c.localhost", "", "", true},
	} {
		a, s, err := parseHost(c.host, "localhost")
		if (err != nil) != c.bad || a != c.actor || s != c.space {
			t.Errorf("%s: got %q %q %v", c.host, a, s, err)
		}
	}
}

func TestTokens(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	tok, _ := signToken(priv, "alice", time.Hour, now)
	if c, err := verifyToken([]ed25519.PublicKey{pub}, tok, now); err != nil || c.Sub != "alice" {
		t.Fatalf("valid token: %v %v", c, err)
	}
	if _, err := verifyToken([]ed25519.PublicKey{otherPub}, tok, now); err == nil {
		t.Error("accepted token signed by another key")
	}
	if _, err := verifyToken([]ed25519.PublicKey{pub}, tok, now.Add(2*time.Hour)); err == nil {
		t.Error("accepted expired token")
	}
	parts := strings.Split(tok, ".")
	forged, _ := signToken(priv, "mallory", time.Hour, now)
	if _, err := verifyToken([]ed25519.PublicKey{pub}, parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], now); err == nil {
		t.Error("accepted claims swapped under another signature")
	}
}

type fakeAuth struct {
	mu sync.Mutex
	ok map[string]bool // principal/atespace
}

func (f *fakeAuth) Allowed(_ context.Context, p, as string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok[p+"/"+as], nil
}

func (f *fakeAuth) set(k string, v bool) { f.mu.Lock(); f.ok[k] = v; f.mu.Unlock() }

type backendDialer struct{ addr string }

func (d backendDialer) dial(ctx context.Context, _, _, _ string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func TestHTTPGateway(t *testing.T) {
	var seenCookie, seenAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCookie, seenAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		if r.URL.Path == "/stream" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // a long-lived response, like the desktop stream
			return
		}
		io.WriteString(w, "desktop")
	}))
	defer backend.Close()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	az := &fakeAuth{ok: map[string]bool{"alice/team-a": true}}
	tr := newTracker()
	gw := httptest.NewServer(newHTTPGateway("localhost", "3000", []ed25519.PublicKey{pub}, az, backendDialer{backend.Listener.Addr().String()}, tr))
	defer gw.Close()

	get := func(host, path, tok string) *http.Response {
		req, _ := http.NewRequest("GET", gw.URL+path, nil)
		req.Host = host
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Cookie", sessionCookie+"="+tok+"; theme=dark")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	alice, _ := signToken(priv, "alice", time.Hour, time.Now())
	bob, _ := signToken(priv, "bob", time.Hour, time.Now())

	if r := get("desk--team-a.localhost", "/", ""); r.StatusCode != 401 {
		t.Errorf("no token: %d", r.StatusCode)
	}
	if r := get("desk--team-a.localhost", "/", bob); r.StatusCode != 403 {
		t.Errorf("non-owner: %d", r.StatusCode)
	}
	if r := get("desk--team-b.localhost", "/", alice); r.StatusCode != 403 {
		t.Errorf("other atespace: %d", r.StatusCode)
	}
	if r := get("example.com", "/", alice); r.StatusCode != 404 {
		t.Errorf("unknown host: %d", r.StatusCode)
	}
	r := get("desk--team-a.localhost", "/", alice)
	if b, _ := io.ReadAll(r.Body); r.StatusCode != 200 || string(b) != "desktop" {
		t.Fatalf("owner: %d %q", r.StatusCode, b)
	}
	if seenAuth != "" || seenCookie != "theme=dark" {
		t.Errorf("credentials leaked to the actor: auth=%q cookie=%q", seenAuth, seenCookie)
	}

	// A login link sets a session cookie.
	req, _ := http.NewRequest("GET", gw.URL+loginPath+"?token="+alice, nil)
	req.Host = "desk--team-a.localhost"
	resp, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if resp.StatusCode != 303 || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly {
		t.Errorf("login: %d %v", resp.StatusCode, resp.Cookies())
	}

	// Revoking access closes a stream that is already open.
	stream := get("desk--team-a.localhost", "/stream", alice)
	az.set("alice/team-a", false)
	tr.sweep(context.Background(), az, time.Now())
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(stream.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after revocation")
	}
}

func TestSweepClosesExpiredTokens(t *testing.T) {
	tr := newTracker()
	a, b := net.Pipe()
	defer b.Close()
	tr.add(a, "alice", "team-a", "desk", time.Now().Add(-time.Second))
	tr.sweep(context.Background(), &fakeAuth{ok: map[string]bool{"alice/team-a": true}}, time.Now())
	if _, err := a.Write([]byte("x")); err == nil {
		t.Error("tunnel stayed open past token expiry")
	}
}
