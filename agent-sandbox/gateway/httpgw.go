package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The HTTP gateway serves desktops. A request for
// <actor>--<atespace>.<domain> is allowed when its token's subject is an
// editor or owner of the atespace (the same rule as SSH), then proxied to the
// actor's port through the router. The desktop has no login of its own.

const loginPath = "/_gw/login"
const sessionCookie = "gw_session"

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseHost extracts actor and atespace from "<actor>--<atespace>.<domain>[:port]".
func parseHost(host, domain string) (actor, atespace string, err error) {
	if h, _, serr := net.SplitHostPort(host); serr == nil {
		host = h
	}
	host = strings.ToLower(host)
	label, ok := strings.CutSuffix(host, "."+domain)
	if !ok {
		return "", "", fmt.Errorf("host %q is not under %s", host, domain)
	}
	actor, atespace, ok = strings.Cut(label, "--")
	if !ok || strings.Contains(atespace, "--") || !dnsLabel.MatchString(actor) || !dnsLabel.MatchString(atespace) {
		return "", "", fmt.Errorf("host %q is not <actor>--<atespace>.%s", host, domain)
	}
	return actor, atespace, nil
}

type authChecker interface {
	Allowed(ctx context.Context, principal, atespace string) (bool, error)
}

// tracker records open tunnels so a revoked or expired user is cut off
// without waiting for the browser to reconnect.
type tracker struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}

type trackedConn struct {
	net.Conn
	t                          *tracker
	principal, atespace, actor string
	expiry                     time.Time
}

func newTracker() *tracker { return &tracker{conns: map[*trackedConn]struct{}{}} }

func (t *tracker) add(c net.Conn, principal, atespace, actor string, expiry time.Time) *trackedConn {
	tc := &trackedConn{Conn: c, t: t, principal: principal, atespace: atespace, actor: actor, expiry: expiry}
	t.mu.Lock()
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	return tc
}

func (c *trackedConn) Close() error {
	c.t.mu.Lock()
	delete(c.t.conns, c)
	c.t.mu.Unlock()
	return c.Conn.Close()
}

// sweep closes tunnels whose token expired or whose owner lost access.
func (t *tracker) sweep(ctx context.Context, az authChecker, now time.Time) {
	t.mu.Lock()
	open := make([]*trackedConn, 0, len(t.conns))
	for c := range t.conns {
		open = append(open, c)
	}
	t.mu.Unlock()
	for _, c := range open {
		if !now.Before(c.expiry) {
			log.Printf("audit expire principal=%s actor=%s/%s: token expired, closing tunnel", c.principal, c.atespace, c.actor)
			c.Close()
			continue
		}
		ok, err := az.Allowed(ctx, c.principal, c.atespace)
		if err != nil || !ok {
			log.Printf("audit revoke principal=%s actor=%s/%s allowed=%v err=%v: closing tunnel", c.principal, c.atespace, c.actor, ok, err)
			c.Close()
		}
	}
}

func (t *tracker) run(ctx context.Context, az authChecker, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			t.sweep(cctx, az, time.Now())
			cancel()
		}
	}
}

type actorDialer interface {
	dial(ctx context.Context, atespace, name, port string) (net.Conn, error)
}

type httpGateway struct {
	domain string
	port   string
	keys   []ed25519.PublicKey
	az     authChecker
	dialer actorDialer
	track  *tracker
	proxy  *httputil.ReverseProxy
}

type reqInfo struct {
	principal, atespace, actor string
	expiry                     time.Time
}

type ctxKey struct{}

func newHTTPGateway(domain, port string, keys []ed25519.PublicKey, az authChecker, d actorDialer, tr *tracker) *httpGateway {
	g := &httpGateway{domain: domain, port: port, keys: keys, az: az, dialer: d, track: tr}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			ri := pr.In.Context().Value(ctxKey{}).(reqInfo)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(ri.actor, g.port)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("Cookie", stripSession(pr.In.Header.Values("Cookie")))
			if pr.Out.Header.Get("Cookie") == "" {
				pr.Out.Header.Del("Cookie")
			}
		},
		// One tunnel per request: a pooled connection dialed for one user must
		// never carry another user's request.
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				ri := ctx.Value(ctxKey{}).(reqInfo)
				c, err := g.dialer.dial(ctx, ri.atespace, ri.actor, g.port)
				if err != nil {
					return nil, err
				}
				log.Printf("audit open  principal=%s actor=%s/%s", ri.principal, ri.atespace, ri.actor)
				return g.track.add(c, ri.principal, ri.atespace, ri.actor, ri.expiry), nil
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ri := r.Context().Value(ctxKey{}).(reqInfo)
			log.Printf("proxy error principal=%s actor=%s/%s: %v", ri.principal, ri.atespace, ri.actor, err)
			http.Error(w, "desktop unreachable (is the actor running?)", http.StatusBadGateway)
		},
	}
	return g
}

// stripSession returns the Cookie header without the gateway's session cookie.
func stripSession(headers []string) string {
	var keep []string
	for _, h := range headers {
		for _, p := range strings.Split(h, ";") {
			p = strings.TrimSpace(p)
			if p != "" && !strings.HasPrefix(p, sessionCookie+"=") {
				keep = append(keep, p)
			}
		}
	}
	return strings.Join(keep, "; ")
}

func bearer(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return t
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (g *httpGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	actor, atespace, err := parseHost(r.Host, g.domain)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == loginPath {
		g.login(w, r)
		return
	}
	tok := bearer(r)
	if tok == "" {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	claims, err := verifyToken(g.keys, tok, time.Now())
	if err != nil {
		http.Error(w, "sign in required: "+err.Error(), http.StatusUnauthorized)
		return
	}
	ok, err := g.az.Allowed(r.Context(), claims.Sub, atespace)
	if err != nil {
		log.Printf("audit deny principal=%s actor=%s/%s err=%v", claims.Sub, atespace, actor, err)
		http.Error(w, "cannot check access", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		log.Printf("audit deny principal=%s actor=%s/%s: not an editor or owner", claims.Sub, atespace, actor)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ri := reqInfo{principal: claims.Sub, atespace: atespace, actor: actor, expiry: time.Unix(claims.Exp, 0)}
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, ri)))
}

// login turns ?token=... into a session cookie for this host and redirects to "/".
func (g *httpGateway) login(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	claims, err := verifyToken(g.keys, tok, time.Now())
	if err != nil {
		http.Error(w, "bad token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(claims.Exp, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var errNoTokenKeys = errors.New("-token-pub is required with -http-listen")
