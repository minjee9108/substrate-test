package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// policy is an AccessPolicy reduced to role -> set of members ("user:<id>").
type policy struct {
	bindings map[string]map[string]bool
}

func (p policy) has(role, member string) bool { return p.bindings[role][member] }

// parsePolicy reads `kubectl ate get access-policy -o json`. Empty input means
// the scope has no policy, which is a policy with no bindings.
func parsePolicy(b []byte) (policy, error) {
	p := policy{bindings: map[string]map[string]bool{}}
	if len(bytes.TrimSpace(b)) == 0 {
		return p, nil
	}
	var doc struct {
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return policy{}, fmt.Errorf("parsing access policy: %w", err)
	}
	for _, bd := range doc.Bindings {
		if p.bindings[bd.Role] == nil {
			p.bindings[bd.Role] = map[string]bool{}
		}
		for _, m := range bd.Members {
			p.bindings[bd.Role][m] = true
		}
	}
	return p, nil
}

// allowed reports whether principal may use actors in an atespace. It mirrors
// cmd/ateapi/internal/authz/model.fga:
//
//	atespace.owner  = [user] or owner from global
//	atespace.editor = [user] or owner
//	actor.can_resume / can_suspend = editor from the atespace
//
// so global owner, atespace owner and atespace editor pass; viewers do not.
// Bootstrap owners come from ate-api-server's flags, not from a policy, so
// this function cannot see them.
func allowed(principal string, global, space policy) bool {
	m := "user:" + principal
	return global.has("owner", m) || space.has("owner", m) || space.has("editor", m)
}

// policySource fetches the policy of one scope; atespace "" is the global scope.
type policySource interface {
	fetch(ctx context.Context, atespace string) (policy, error)
}

// kubectlSource shells out to kubectl-ate. PROTOTYPE: it acts as whatever
// identity kubectl-ate uses (the shared ate-client). A deployed gateway would
// call GetGlobalAccessPolicy / GetAtespaceAccessPolicy over gRPC as its own
// identity, holding global viewer and nothing more.
type kubectlSource struct{ bin, kubeContext string }

func (k kubectlSource) fetch(ctx context.Context, atespace string) (policy, error) {
	args := []string{"--context", k.kubeContext, "get", "access-policy", "-o", "json"}
	if atespace == "" {
		args = append(args, "--global")
	} else {
		args = append(args, "-a", atespace)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, k.bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(lastLine(stderr.String()))
		return policy{}, fmt.Errorf("reading access policy (%s): %v: %s", scopeName(atespace), err, msg)
	}
	return parsePolicy(stdout.Bytes())
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func scopeName(atespace string) string {
	if atespace == "" {
		return "global"
	}
	return "atespace " + atespace
}

// authorizer caches policies for ttl so a burst of connections reads each
// policy once. A revocation therefore takes effect within ttl. Any error
// reading a policy denies (fail closed); a stale cached policy is never used
// to allow after its ttl.
type authorizer struct {
	src policySource
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]entry
	locks map[string]*sync.Mutex
}

type entry struct {
	p  policy
	at time.Time
}

func newAuthorizer(src policySource, ttl time.Duration) *authorizer {
	return &authorizer{src: src, ttl: ttl, now: time.Now, cache: map[string]entry{}, locks: map[string]*sync.Mutex{}}
}

func (a *authorizer) policy(ctx context.Context, atespace string) (policy, error) {
	a.mu.Lock()
	l := a.locks[atespace]
	if l == nil {
		l = &sync.Mutex{}
		a.locks[atespace] = l
	}
	a.mu.Unlock()
	// One fetch per scope at a time; concurrent callers reuse its result.
	l.Lock()
	defer l.Unlock()

	a.mu.Lock()
	e, ok := a.cache[atespace]
	a.mu.Unlock()
	if ok && a.now().Sub(e.at) < a.ttl {
		return e.p, nil
	}
	p, err := a.src.fetch(ctx, atespace)
	if err != nil {
		return policy{}, err
	}
	a.mu.Lock()
	a.cache[atespace] = entry{p: p, at: a.now()}
	a.mu.Unlock()
	return p, nil
}

// Allowed returns (false, nil) for a definite denial and a non-nil error when
// it could not decide; callers must treat an error as a denial for new access.
func (a *authorizer) Allowed(ctx context.Context, principal, atespace string) (bool, error) {
	g, err := a.policy(ctx, "")
	if err != nil {
		return false, err
	}
	s, err := a.policy(ctx, atespace)
	if err != nil {
		return false, err
	}
	return allowed(principal, g, s), nil
}
