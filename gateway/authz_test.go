package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func pol(t *testing.T, js string) policy {
	t.Helper()
	p, err := parsePolicy([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Cases mirror cmd/ateapi/internal/authz/model_test.fga.yaml: carol is global
// owner, frank global viewer, alice atespace owner, bob editor, dave viewer.
func TestAllowed(t *testing.T) {
	global := pol(t, `{"bindings":[{"role":"owner","members":["user:carol"]},{"role":"viewer","members":["user:frank"]}]}`)
	space := pol(t, `{"bindings":[
	  {"role":"owner","members":["user:alice"]},
	  {"role":"editor","members":["user:bob"]},
	  {"role":"viewer","members":["user:dave"]}]}`)
	none := pol(t, ``)

	tests := []struct {
		name      string
		principal string
		global    policy
		space     policy
		want      bool
	}{
		{"global owner", "carol", global, none, true},
		{"global viewer cannot resume", "frank", global, none, false},
		{"atespace owner", "alice", global, space, true},
		{"atespace editor", "bob", global, space, true},
		{"atespace viewer cannot resume", "dave", global, space, false},
		{"unbound principal", "mallory", global, space, false},
		{"no policies at all", "alice", none, none, false},
		{"prefix is not a match", "alic", global, space, false},
		{"member without user: prefix is ignored", "alice", none, pol(t, `{"bindings":[{"role":"editor","members":["alice"]}]}`), false},
		{"principal with colons (service account)", "system:serviceaccount:team-a:alice", none,
			pol(t, `{"bindings":[{"role":"editor","members":["user:system:serviceaccount:team-a:alice"]}]}`), true},
		{"global owner reaches an atespace with no policy", "carol", global, none, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowed(tt.principal, tt.global, tt.space); got != tt.want {
				t.Fatalf("allowed(%q) = %v, want %v", tt.principal, got, tt.want)
			}
		})
	}
}

func TestParsePolicy(t *testing.T) {
	if p, err := parsePolicy(nil); err != nil || len(p.bindings) != 0 {
		t.Fatalf("empty input should be an empty policy, got %v %v", p, err)
	}
	if _, err := parsePolicy([]byte("{not json")); err == nil {
		t.Fatal("garbage must be an error, not an empty policy")
	}
}

type fakeSource struct {
	policies map[string]policy
	err      error
	calls    map[string]int
}

func (f *fakeSource) fetch(_ context.Context, atespace string) (policy, error) {
	f.calls[atespace]++
	if f.err != nil {
		return policy{}, f.err
	}
	return f.policies[atespace], nil
}

func newFake(t *testing.T) *fakeSource {
	return &fakeSource{calls: map[string]int{}, policies: map[string]policy{
		"":     pol(t, ``),
		"team": pol(t, `{"bindings":[{"role":"editor","members":["user:alice"]}]}`),
	}}
}

func TestAuthorizerCachesWithinTTLAndRefreshesAfter(t *testing.T) {
	src := newFake(t)
	clock := time.Unix(1000, 0)
	a := newAuthorizer(src, 30*time.Second)
	a.now = func() time.Time { return clock }
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if ok, err := a.Allowed(ctx, "alice", "team"); !ok || err != nil {
			t.Fatalf("want allowed, got %v %v", ok, err)
		}
	}
	if src.calls["team"] != 1 || src.calls[""] != 1 {
		t.Fatalf("policies should be read once inside the ttl, got %v", src.calls)
	}

	// Revoke alice at the source; the cache still allows until ttl passes.
	src.policies["team"] = pol(t, ``)
	if ok, _ := a.Allowed(ctx, "alice", "team"); !ok {
		t.Fatal("revocation must not apply before the ttl expires")
	}
	clock = clock.Add(31 * time.Second)
	if ok, err := a.Allowed(ctx, "alice", "team"); ok || err != nil {
		t.Fatalf("revocation must apply after the ttl, got %v %v", ok, err)
	}
}

func TestAuthorizerFailsClosed(t *testing.T) {
	src := newFake(t)
	clock := time.Unix(1000, 0)
	a := newAuthorizer(src, 30*time.Second)
	a.now = func() time.Time { return clock }
	ctx := context.Background()

	if ok, _ := a.Allowed(ctx, "alice", "team"); !ok {
		t.Fatal("setup: alice should be allowed")
	}
	// The source breaks after the ttl: a previously-allowed user must not get in
	// on stale data.
	src.err = errors.New("ate-api unreachable")
	clock = clock.Add(31 * time.Second)
	ok, err := a.Allowed(ctx, "alice", "team")
	if ok || err == nil {
		t.Fatalf("an unreadable policy must deny with an error, got ok=%v err=%v", ok, err)
	}
}
