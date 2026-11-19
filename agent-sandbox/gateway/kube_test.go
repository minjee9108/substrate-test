package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeChecker struct {
	calls atomic.Int32
	allow bool
	err   error
}

func (f *fakeChecker) canUpdateSandboxes(context.Context, string, string) (bool, error) {
	f.calls.Add(1)
	return f.allow, f.err
}

func TestAuthorizerCachesAndExpires(t *testing.T) {
	f := &fakeChecker{allow: true}
	a := newAuthorizer(f, time.Minute)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }
	for range 3 {
		if ok, err := a.Allowed(context.Background(), "alice", "team-a"); !ok || err != nil {
			t.Fatalf("want allowed, got %v %v", ok, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("want 1 lookup within ttl, got %d", f.calls.Load())
	}
	f.allow = false
	now = now.Add(2 * time.Minute)
	if ok, _ := a.Allowed(context.Background(), "alice", "team-a"); ok {
		t.Fatal("revocation must apply after ttl")
	}
	if ok, _ := a.Allowed(context.Background(), "alice", "team-b"); ok {
		t.Fatal("decisions are per namespace")
	}
}

func TestAuthorizerFailsClosed(t *testing.T) {
	f := &fakeChecker{allow: true}
	a := newAuthorizer(f, time.Minute)
	now := time.Unix(1000, 0)
	a.now = func() time.Time { return now }
	if ok, _ := a.Allowed(context.Background(), "alice", "team-a"); !ok {
		t.Fatal("want allowed")
	}
	now = now.Add(2 * time.Minute)
	f.err = errors.New("api down")
	if ok, err := a.Allowed(context.Background(), "alice", "team-a"); ok || err == nil {
		t.Fatalf("an expired entry must not allow when the lookup fails, got %v %v", ok, err)
	}
}

func fakeKube(t *testing.T, h http.HandlerFunc) *kubeClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &kubeClient{base: srv.URL, tokenFile: tok, hc: srv.Client()}
}

func TestSubjectAccessReview(t *testing.T) {
	k := fakeKube(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		if r.Header.Get("Authorization") != "Bearer tok" || !strings.Contains(body, `"user":"alice"`) || !strings.Contains(body, `"resource":"sandboxes"`) {
			t.Errorf("unexpected request: %s %q", r.Header.Get("Authorization"), body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":{"allowed":true}}`))
	})
	if ok, err := k.canUpdateSandboxes(context.Background(), "alice", "team-a"); !ok || err != nil {
		t.Fatalf("want allowed, got %v %v", ok, err)
	}
}

func TestSubjectAccessReviewErrorIsNotAllowed(t *testing.T) {
	k := fakeKube(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if ok, err := k.canUpdateSandboxes(context.Background(), "alice", "team-a"); ok || err == nil {
		t.Fatalf("want error, got %v %v", ok, err)
	}
}

func TestEnsureRunningResumesSuspended(t *testing.T) {
	var patched atomic.Bool
	k := fakeKube(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			patched.Store(true)
			w.Write([]byte(`{}`))
		case http.MethodGet:
			if !patched.Load() {
				w.Write([]byte(`{"spec":{"operatingMode":"Suspended"}}`))
				return
			}
			w.Write([]byte(`{"spec":{"operatingMode":"Running"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := k.ensureRunning(ctx, "team-a", "ssh-1"); err != nil {
		t.Fatal(err)
	}
	if !patched.Load() {
		t.Fatal("a suspended sandbox must be resumed")
	}
}

func TestEnsureRunningMissingSandbox(t *testing.T) {
	k := fakeKube(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	err := k.ensureRunning(context.Background(), "team-a", "nope")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want does-not-exist error, got %v", err)
	}
}
