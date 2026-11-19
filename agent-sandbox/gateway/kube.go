package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	saDir          = "/var/run/secrets/kubernetes.io/serviceaccount"
	sandboxGroup   = "agents.x-k8s.io"
	sandboxVersion = "v1beta1"
)

var errSandboxNotFound = errors.New("sandbox not found")

// kubeClient is the few Kubernetes API calls the gateway needs, over plain HTTPS
// with the pod's ServiceAccount token.
type kubeClient struct {
	base      string
	tokenFile string
	hc        *http.Client
}

func newInClusterKube() (*kubeClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST is unset")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificates in ca.crt")
	}
	return &kubeClient{
		base:      "https://" + host + ":" + port,
		tokenFile: saDir + "/token",
		hc:        &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (k *kubeClient) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, int, error) {
	tok, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

// canUpdateSandboxes asks the API server whether user may update Sandboxes in namespace.
// That is the access check: whoever Kubernetes RBAC lets edit Sandboxes may connect.
func (k *kubeClient) canUpdateSandboxes(ctx context.Context, user, namespace string) (bool, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SubjectAccessReview",
		"spec": map[string]any{
			"user": user,
			"resourceAttributes": map[string]any{
				"namespace": namespace, "verb": "update", "group": sandboxGroup, "resource": "sandboxes",
			},
		},
	})
	b, code, err := k.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/subjectaccessreviews", "application/json", body)
	if err != nil {
		return false, err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return false, fmt.Errorf("subjectaccessreview: HTTP %d: %s", code, firstLine(b))
	}
	var r struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return false, fmt.Errorf("subjectaccessreview: %w", err)
	}
	return r.Status.Allowed, nil
}

type sandboxState struct {
	suspended bool
	ready     bool
}

func (k *kubeClient) getSandbox(ctx context.Context, namespace, name string) (sandboxState, error) {
	path := fmt.Sprintf("/apis/%s/%s/namespaces/%s/sandboxes/%s", sandboxGroup, sandboxVersion, namespace, name)
	b, code, err := k.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return sandboxState{}, err
	}
	if code == http.StatusNotFound {
		return sandboxState{}, errSandboxNotFound
	}
	if code != http.StatusOK {
		return sandboxState{}, fmt.Errorf("get sandbox: HTTP %d: %s", code, firstLine(b))
	}
	var o struct {
		Spec struct {
			OperatingMode string `json:"operatingMode"`
		} `json:"spec"`
		Status struct {
			Conditions []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return sandboxState{}, err
	}
	st := sandboxState{suspended: o.Spec.OperatingMode == "Suspended"}
	for _, c := range o.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			st.ready = true
		}
	}
	return st, nil
}

func (k *kubeClient) resume(ctx context.Context, namespace, name string) error {
	path := fmt.Sprintf("/apis/%s/%s/namespaces/%s/sandboxes/%s", sandboxGroup, sandboxVersion, namespace, name)
	b, code, err := k.do(ctx, http.MethodPatch, path, "application/merge-patch+json", []byte(`{"spec":{"operatingMode":"Running"}}`))
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("resume sandbox: HTTP %d: %s", code, firstLine(b))
	}
	return nil
}

// ensureRunning returns once the sandbox exists and is Ready. It resumes a suspended sandbox first.
func (k *kubeClient) ensureRunning(ctx context.Context, namespace, name string) error {
	st, err := k.getSandbox(ctx, namespace, name)
	if errors.Is(err, errSandboxNotFound) {
		return fmt.Errorf("sandbox %s/%s does not exist", namespace, name)
	}
	if err != nil {
		return err
	}
	if st.suspended {
		log.Printf("audit resume sandbox=%s/%s", namespace, name)
		if err := k.resume(ctx, namespace, name); err != nil {
			return err
		}
		st.ready = false
	}
	for !st.ready {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s/%s not ready: %w", namespace, name, ctx.Err())
		case <-time.After(time.Second):
		}
		if st, err = k.getSandbox(ctx, namespace, name); err != nil {
			return err
		}
	}
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// accessChecker is what authorizer needs from the Kubernetes client.
type accessChecker interface {
	canUpdateSandboxes(ctx context.Context, user, namespace string) (bool, error)
}

type decision struct {
	allowed bool
	at      time.Time
}

// authorizer caches RBAC decisions for ttl. A revocation takes effect within ttl.
// An error reading the decision denies (fail closed), and an expired entry is
// never used to allow.
type authorizer struct {
	src accessChecker
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]decision
}

func newAuthorizer(src accessChecker, ttl time.Duration) *authorizer {
	return &authorizer{src: src, ttl: ttl, now: time.Now, cache: map[string]decision{}}
}

// Allowed returns (false, nil) for a definite denial and a non-nil error when it
// could not decide. Callers must treat an error as a denial for new access.
func (a *authorizer) Allowed(ctx context.Context, principal, namespace string) (bool, error) {
	key := principal + "\x00" + namespace
	a.mu.Lock()
	defer a.mu.Unlock() // one lookup at a time keeps a burst of connections to one API call
	if d, ok := a.cache[key]; ok && a.now().Sub(d.at) < a.ttl {
		return d.allowed, nil
	}
	ok, err := a.src.canUpdateSandboxes(ctx, principal, namespace)
	if err != nil {
		delete(a.cache, key)
		return false, err
	}
	a.cache[key] = decision{allowed: ok, at: a.now()}
	return ok, nil
}
