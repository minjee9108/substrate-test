// ssh-gateway prototype (stage 1, local): terminates the user's SSH connection,
// authorizes <actor>.<atespace> from the SSH username, then opens a second SSH
// connection to the actor's sshd through `kubectl ate proxy` + WebSocket and
// proxies every channel between the two.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

var (
	listen      = flag.String("listen", "127.0.0.1:2222", "listen address")
	hostKeyPath = flag.String("host-key", "hostkey", "gateway SSH host key")
	usersPath   = flag.String("users", "", "static key registry: <pubkey> <principal> per line (optional; certificates are preferred)")
	caPath      = flag.String("ca-pub", "", "trusted SSH CA public key(s), one per line; enables certificate login")
	pinsPath    = flag.String("actor-hostkeys", "upstream_hostkeys.json", "file recording each actor's pinned SSH host key")
	maxCertLife = flag.Duration("max-cert-lifetime", 24*time.Hour, "reject certificates valid for longer than this")
	upKeyPath   = flag.String("upstream-key", "upstream_key", "key the actor's sshd trusts")
	kubectlAte  = flag.String("kubectl-ate", os.ExpandEnv("$HOME/go/bin/kubectl-ate"), "kubectl-ate binary")
	kubeCtx     = flag.String("context", "kind-kind", "kube context")
	policyTTL   = flag.Duration("policy-ttl", 30*time.Second, "how long an access policy is cached; revocations apply within this")
	recheck     = flag.Duration("recheck", 30*time.Second, "how often live sessions are re-authorized")

	// In-cluster mode: talk to the router and ate-api directly, with the pod's own identity.
	routerAddr   = flag.String("router-addr", "", "atenet-router CONNECT-TLS host:port; enables direct (in-cluster) dialing instead of kubectl-ate")
	routerName   = flag.String("router-server-name", "atenet-router.ate-system.svc", "router serving certificate name")
	routerCA     = flag.String("router-ca", "/run/trust/servicedns/trust-bundle.pem", "CA bundle that signs the router's serving certificate")
	routerCred   = flag.String("router-cert", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "this pod's podidentity client certificate and key")
	apiAddr      = flag.String("api-addr", "", "ate-api host:port; enables reading access policies over gRPC instead of kubectl-ate")
	apiName      = flag.String("api-server-name", "api.ate-system.svc", "ate-api serving certificate name")
	apiCA        = flag.String("api-ca", "/run/trust/servicedns/trust-bundle.pem", "CA bundle that signs ate-api's serving certificate")
	apiTokenFile = flag.String("api-token", "/var/run/secrets/ate/token", "ServiceAccount token (audience api.ate-system.svc) sent to ate-api")
)

var (
	authz  *authorizer
	pins   *pinStore
	router *routerDialer // nil: fall back to kubectl-ate proxy
)

// user is a registered SSH key and the Substrate principal it acts as.
// What the principal may do comes from Substrate access policies, not from here.
type user struct {
	principal string
}

func loadUsers(path string) (map[string]user, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	users := map[string]user{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(comment) == "" {
			return nil, fmt.Errorf("users: key %s has no principal", ssh.FingerprintSHA256(pk))
		}
		users[string(pk.Marshal())] = user{principal: strings.TrimSpace(comment)}
	}
	return users, sc.Err()
}

var targetRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)\.([a-z0-9]([-a-z0-9]*[a-z0-9])?)$`)

// parseTarget turns the SSH username "<actor>.<atespace>" into its parts.
func parseTarget(username string) (actor, atespace string, err error) {
	m := targetRE.FindStringSubmatch(username)
	if m == nil {
		return "", "", fmt.Errorf("username must be <actor>.<atespace>, got %q", username)
	}
	return m[1], m[3], nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "issue" {
		issueMain(os.Args[2:])
		return
	}
	flag.Parse()
	users := map[string]user{}
	if *usersPath != "" {
		var err error
		if users, err = loadUsers(*usersPath); err != nil {
			log.Fatalf("users: %v", err)
		}
	}
	var perr error
	if pins, perr = loadPins(*pinsPath); perr != nil {
		log.Fatalf("actor host keys: %v", perr)
	}
	var checker *ssh.CertChecker
	if *caPath != "" {
		cas, err := loadCAs(*caPath)
		if err != nil {
			log.Fatalf("ca: %v", err)
		}
		checker = newCertChecker(cas)
	}
	if checker == nil && len(users) == 0 {
		log.Fatal("no way to authenticate users: set -ca-pub and/or -users")
	}
	hk, err := readSigner(*hostKeyPath)
	if err != nil {
		log.Fatalf("host key: %v", err)
	}
	var src policySource = kubectlSource{bin: *kubectlAte, kubeContext: *kubeCtx}
	if *apiAddr != "" {
		g, err := newGRPCSource(*apiAddr, *apiCA, *apiName, *apiTokenFile)
		if err != nil {
			log.Fatalf("ate-api client: %v", err)
		}
		src = g
	}
	if *routerAddr != "" {
		var err error
		if router, err = newRouterDialer(*routerAddr, *routerName, *routerCA, *routerCred); err != nil {
			log.Fatalf("router client: %v", err)
		}
	}
	authz = newAuthorizer(src, *policyTTL)
	upSigner, err := readSigner(*upKeyPath)
	if err != nil {
		log.Fatalf("upstream key: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			var principal string
			var expiry time.Time // zero: the credential does not expire
			if cert, isCert := key.(*ssh.Certificate); isCert {
				if checker == nil {
					return nil, fmt.Errorf("certificate login is not enabled")
				}
				var err error
				if principal, expiry, err = authenticateCert(checker, cert, *maxCertLife); err != nil {
					return nil, fmt.Errorf("certificate rejected: %v", err)
				}
			} else if u, ok := users[string(key.Marshal())]; ok {
				principal = u.principal
			} else {
				return nil, fmt.Errorf("key is neither a valid certificate nor registered")
			}
			actor, atespace, err := parseTarget(c.User())
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			permitted, err := authz.Allowed(ctx, principal, atespace)
			if err != nil {
				// Could not read the policies: deny (fail closed).
				return nil, fmt.Errorf("cannot authorize %s for %s: %v", principal, atespace, err)
			}
			if !permitted {
				return nil, fmt.Errorf("%s is not an editor or owner of atespace %s", principal, atespace)
			}
			exp := ""
			if !expiry.IsZero() {
				exp = expiry.Format(time.RFC3339)
			}
			return &ssh.Permissions{Extensions: map[string]string{
				"principal": principal, "actor": actor, "atespace": atespace, "expiry": exp,
			}}, nil
		},
	}
	cfg.AddHostKey(hk)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("ssh-gateway listening on %s", *listen)
	for {
		nc, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handleConn(nc, cfg, upSigner)
	}
}

func readSigner(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func handleConn(nc net.Conn, cfg *ssh.ServerConfig, upSigner ssh.Signer) {
	defer nc.Close()
	start := time.Now()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		log.Printf("audit deny remote=%s err=%v", nc.RemoteAddr(), err)
		return
	}
	defer sc.Close()
	who, actor, atespace := sc.Permissions.Extensions["principal"], sc.Permissions.Extensions["actor"], sc.Permissions.Extensions["atespace"]
	log.Printf("audit open  principal=%s actor=%s/%s remote=%s cert-expires=%q", who, atespace, actor, nc.RemoteAddr(), sc.Permissions.Extensions["expiry"])
	if exp, err := time.Parse(time.RFC3339, sc.Permissions.Extensions["expiry"]); err == nil {
		// A session never outlives the credential that opened it.
		t := time.AfterFunc(time.Until(exp), func() {
			log.Printf("audit expire principal=%s actor=%s/%s: certificate expired, closing session", who, atespace, actor)
			sc.Close()
		})
		defer t.Stop()
	}
	defer func() {
		log.Printf("audit close principal=%s actor=%s/%s duration=%s", who, atespace, actor, time.Since(start).Round(time.Millisecond))
	}()

	up, cleanup, err := dialUpstream(atespace, actor, upSigner)
	if err != nil {
		log.Printf("audit upstream-fail principal=%s actor=%s/%s err=%v", who, atespace, actor, err)
		// Surface the reason to the user's terminal via a rejected channel.
		go ssh.DiscardRequests(reqs)
		for nch := range chans {
			nch.Reject(ssh.ConnectionFailed, "gateway: cannot reach actor: "+err.Error())
		}
		return
	}
	defer cleanup()

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(*recheck)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				ok, err := authz.Allowed(ctx, who, atespace)
				cancel()
				if err != nil {
					// Cannot tell; keep the session and try again next tick.
					log.Printf("audit recheck-error principal=%s actor=%s/%s err=%v", who, atespace, actor, err)
				} else if !ok {
					log.Printf("audit revoke principal=%s actor=%s/%s: no longer authorized, closing session", who, atespace, actor)
					sc.Close()
					return
				}
			}
		}
	}()

	go func() {
		for r := range reqs {
			// Answer OpenSSH keepalives; refuse other global requests (e.g. remote forwards).
			r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}()
	// Channels the actor opens towards the user (agent, x11, forwarded-tcpip)
	// are not handled, so the SSH client library rejects them.

	var wg sync.WaitGroup
	for nch := range chans {
		switch nch.ChannelType() {
		case "session", "direct-tcpip":
			wg.Add(1)
			go func() { defer wg.Done(); proxyChannel(nch, up) }()
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
	wg.Wait()
}

// dialUpstream starts `kubectl ate proxy` for the actor, upgrades to the
// actor's /ssh WebSocket, and runs an SSH client over it as user "dev".
func dialUpstream(atespace, actor string, signer ssh.Signer) (*ssh.Client, func(), error) {
	kill := func() {}
	wsURL := ""
	var httpClient *http.Client
	if router != nil {
		// Direct: one CONNECT tunnel through the router, HTTP/WebSocket inside it.
		dctx, dcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dcancel()
		tun, err := router.dial(dctx, atespace, actor, "80")
		if err != nil {
			return nil, nil, err
		}
		kill = func() { tun.Close() }
		wsURL = "ws://" + actor + ":80/ssh"
		httpClient = &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return tun, nil },
		}}
	} else {
		var port string
		var err error
		if port, kill, err = startKubectlProxy(atespace, actor); err != nil {
			return nil, nil, err
		}
		wsURL = "ws://127.0.0.1:" + port + "/ssh"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		kill()
		return nil, nil, fmt.Errorf("websocket: %w", err)
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	ccfg := &ssh.ClientConfig{
		User: "dev",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// Pin each actor's host key on first contact; refuse a changed key.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			first, err := pins.verify(atespace+"/"+actor, key)
			if err != nil {
				log.Printf("audit hostkey-mismatch actor=%s/%s err=%v", atespace, actor, err)
				return err
			}
			if first {
				log.Printf("audit hostkey-pinned actor=%s/%s fingerprint=%s", atespace, actor, ssh.FingerprintSHA256(key))
			}
			return nil
		},
		Timeout: 60 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, actor, ccfg)
	if err != nil {
		conn.Close()
		kill()
		return nil, nil, fmt.Errorf("upstream ssh: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	return client, func() { client.Close(); kill() }, nil
}

// proxyChannel opens the same channel type on the upstream connection and
// relays data, extended data and channel requests in both directions.
func proxyChannel(newCh ssh.NewChannel, up *ssh.Client) {
	upCh, upReqs, err := up.OpenChannel(newCh.ChannelType(), newCh.ExtraData())
	if err != nil {
		if oe, ok := err.(*ssh.OpenChannelError); ok {
			newCh.Reject(oe.Reason, oe.Message)
		} else {
			newCh.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		upCh.Close()
		return
	}
	relay := func(in <-chan *ssh.Request, out ssh.Channel, done func()) {
		defer done()
		for r := range in {
			ok, err := out.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
		}
	}
	var upstreamSide sync.WaitGroup // everything that flows actor -> user
	upstreamSide.Add(3)
	go relay(reqs, upCh, func() {})
	go relay(upReqs, ch, upstreamSide.Done)
	go func() { defer upstreamSide.Done(); io.Copy(ch, upCh); ch.CloseWrite() }()
	go func() { defer upstreamSide.Done(); io.Copy(ch.Stderr(), upCh.Stderr()) }()
	go func() { io.Copy(upCh, ch); upCh.CloseWrite() }()
	upstreamSide.Wait()
	ch.Close()
	upCh.Close()
}

// startKubectlProxy is the prototype path: run `kubectl ate proxy` for the actor
// and return its local port and a function that stops it.
func startKubectlProxy(atespace, actor string) (string, func(), error) {
	cmd := exec.Command(*kubectlAte, "--context", *kubeCtx, "proxy", "actor", actor, "-a", atespace, ":80")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	kill := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	portCh := make(chan string, 1)
	errBuf := &strings.Builder{}
	go func() {
		re := regexp.MustCompile(`Proxying 127\.0\.0\.1:(\d+) `)
		sc := bufio.NewScanner(stderr)
		sent := false
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil && !sent {
				portCh <- m[1]
				sent = true
			} else if !sent {
				errBuf.WriteString(sc.Text() + "\n")
			}
		}
		if !sent {
			close(portCh)
		}
	}()
	select {
	case p, ok := <-portCh:
		if !ok {
			kill()
			return "", nil, fmt.Errorf("proxy failed: %s", strings.TrimSpace(errBuf.String()))
		}
		return p, kill, nil
	case <-time.After(30 * time.Second):
		kill()
		return "", nil, fmt.Errorf("proxy did not start")
	}
}
