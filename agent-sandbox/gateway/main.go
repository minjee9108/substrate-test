// ssh-gateway for agent-sandbox. It ends the user's SSH connection and takes the
// target from the SSH username, in the form <sandbox>.<namespace>. It allows the
// user if Kubernetes RBAC lets them update Sandboxes in that namespace, resumes a
// suspended Sandbox, and opens a second SSH connection to the sandbox's sshd. Then
// it copies every channel between the two connections.
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
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	listen      = flag.String("listen", ":2222", "listen address")
	hostKeyPath = flag.String("host-key", "hostkey", "gateway SSH host key")
	usersPath   = flag.String("users", "", "static key registry: <pubkey> <principal> per line (optional; certificates are preferred)")
	caPath      = flag.String("ca-pub", "", "trusted SSH CA public key(s), one per line; enables certificate login")
	pinsPath    = flag.String("sandbox-hostkeys", "sandbox_hostkeys.json", "file recording each sandbox's pinned SSH host key")
	maxCertLife = flag.Duration("max-cert-lifetime", 24*time.Hour, "reject certificates valid for longer than this")
	upKeyPath   = flag.String("upstream-key", "upstream_key", "key the sandbox's sshd trusts")
	authzTTL    = flag.Duration("authz-ttl", 30*time.Second, "how long an RBAC decision is cached; revocations apply within this")
	recheck     = flag.Duration("recheck", 30*time.Second, "how often live sessions are re-authorized")
	sshPort     = flag.String("sandbox-ssh-port", "22", "sshd port in the sandbox")
	httpListen  = flag.String("http-listen", "", "address of the desktop HTTP gateway (disabled if empty)")
	httpDomain  = flag.String("http-domain", "localhost", "desktops are served at <sandbox>--<namespace>.<this domain>")
	httpPort    = flag.String("http-sandbox-port", "3000", "sandbox port the desktop listens on")
	tokenPub    = flag.String("token-pub", "", "base64 Ed25519 public keys that verify desktop login tokens")
	readyWait   = flag.Duration("ready-timeout", 120*time.Second, "how long to wait for a resumed sandbox to become ready")
)

var (
	authz *authorizer
	pins  *pinStore
	kube  *kubeClient
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

// parseTarget turns the SSH username "<sandbox>.<namespace>" into its parts.
func parseTarget(username string) (sandbox, namespace string, err error) {
	m := targetRE.FindStringSubmatch(username)
	if m == nil {
		return "", "", fmt.Errorf("username must be <sandbox>.<namespace>, got %q", username)
	}
	return m[1], m[3], nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "issue" {
		issueMain(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "issue-token" {
		issueTokenMain(os.Args[2:])
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
		log.Fatalf("sandbox host keys: %v", perr)
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
	kc, err := newInClusterKube()
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	kube = kc
	authz = newAuthorizer(kc, *authzTTL)
	if *httpListen != "" {
		if *tokenPub == "" {
			log.Fatal(errNoTokenKeys)
		}
		keys, err := loadTokenKeys(*tokenPub)
		if err != nil {
			log.Fatalf("token keys: %v", err)
		}
		tr := newTracker()
		go tr.run(context.Background(), authz, *recheck)
		hg := newHTTPGateway(*httpDomain, *httpPort, keys, authz, svcDialer{kube: kc, readyWait: *readyWait}, tr)
		go func() {
			log.Printf("desktop http gateway listening on %s for *.%s", *httpListen, *httpDomain)
			log.Fatal((&http.Server{Addr: *httpListen, Handler: hg, ReadHeaderTimeout: 20 * time.Second}).ListenAndServe())
		}()
	}
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
			sandbox, namespace, err := parseTarget(c.User())
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			permitted, err := authz.Allowed(ctx, principal, namespace)
			if err != nil {
				// Could not read the policies: deny (fail closed).
				return nil, fmt.Errorf("cannot authorize %s for %s: %v", principal, namespace, err)
			}
			if !permitted {
				return nil, fmt.Errorf("%s may not update sandboxes in namespace %s", principal, namespace)
			}
			exp := ""
			if !expiry.IsZero() {
				exp = expiry.Format(time.RFC3339)
			}
			return &ssh.Permissions{Extensions: map[string]string{
				"principal": principal, "sandbox": sandbox, "namespace": namespace, "expiry": exp,
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
	who, sandbox, namespace := sc.Permissions.Extensions["principal"], sc.Permissions.Extensions["sandbox"], sc.Permissions.Extensions["namespace"]
	log.Printf("audit open  principal=%s sandbox=%s/%s remote=%s cert-expires=%q", who, namespace, sandbox, nc.RemoteAddr(), sc.Permissions.Extensions["expiry"])
	if exp, err := time.Parse(time.RFC3339, sc.Permissions.Extensions["expiry"]); err == nil {
		// A session never outlives the credential that opened it.
		t := time.AfterFunc(time.Until(exp), func() {
			log.Printf("audit expire principal=%s sandbox=%s/%s: certificate expired, closing session", who, namespace, sandbox)
			sc.Close()
		})
		defer t.Stop()
	}
	defer func() {
		log.Printf("audit close principal=%s sandbox=%s/%s duration=%s", who, namespace, sandbox, time.Since(start).Round(time.Millisecond))
	}()

	up, cleanup, err := dialUpstream(namespace, sandbox, upSigner)
	if err != nil {
		log.Printf("audit upstream-fail principal=%s sandbox=%s/%s err=%v", who, namespace, sandbox, err)
		// Surface the reason to the user's terminal via a rejected channel.
		go ssh.DiscardRequests(reqs)
		for nch := range chans {
			nch.Reject(ssh.ConnectionFailed, "gateway: cannot reach sandbox: "+err.Error())
		}
		return
	}
	defer cleanup()
	go func() {
		err := up.Wait()
		log.Printf("audit upstream-closed principal=%s sandbox=%s/%s after=%s err=%v", who, namespace, sandbox, time.Since(start).Round(time.Millisecond), err)
	}()

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
				ok, err := authz.Allowed(ctx, who, namespace)
				cancel()
				if err != nil {
					// Cannot tell; keep the session and try again next tick.
					log.Printf("audit recheck-error principal=%s sandbox=%s/%s err=%v", who, namespace, sandbox, err)
				} else if !ok {
					log.Printf("audit revoke principal=%s sandbox=%s/%s: no longer authorized, closing session", who, namespace, sandbox)
					sc.Close()
					return
				}
			}
		}
	}()

	go func() {
		for r := range reqs {
			// Answer OpenSSH keepalives; refuse other global requests (e.g. remote forwards).
			if r.Type != "keepalive@openssh.com" {
				log.Printf("audit global-request principal=%s type=%s refused", who, r.Type)
			}
			r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}()
	// Channels the sandbox opens towards the user (agent, x11, forwarded-tcpip)
	// are not handled, so the SSH client library rejects them.

	var wg sync.WaitGroup
	for nch := range chans {
		switch nch.ChannelType() {
		case "session", "direct-tcpip":
			wg.Add(1)
			go func() { defer wg.Done(); proxyChannel(nch, up) }()
		default:
			log.Printf("audit channel-rejected principal=%s type=%s", who, nch.ChannelType())
			nch.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
	wg.Wait()
	log.Printf("audit user-side-closed principal=%s sandbox=%s/%s after=%s err=%v", who, namespace, sandbox, time.Since(start).Round(time.Millisecond), sc.Wait())
}

// dialUpstream makes sure the sandbox is running, opens a TCP connection to its
// sshd through the headless Service, and runs an SSH client over it as user "dev".
func dialUpstream(namespace, sandbox string, signer ssh.Signer) (*ssh.Client, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), *readyWait)
	defer cancel()
	if err := kube.ensureRunning(ctx, namespace, sandbox); err != nil {
		return nil, nil, err
	}
	addr := net.JoinHostPort(fmt.Sprintf("%s.%s.svc", sandbox, namespace), *sshPort)
	var conn net.Conn
	var err error
	for {
		d := net.Dialer{Timeout: 5 * time.Second}
		if conn, err = d.DialContext(ctx, "tcp", addr); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("dialing %s: %w", addr, err)
		case <-time.After(time.Second):
		}
	}
	ccfg := &ssh.ClientConfig{
		User: "dev",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// Pin each sandbox's host key on first contact and refuse a changed key.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			first, err := pins.verify(namespace+"/"+sandbox, key)
			if err != nil {
				log.Printf("audit hostkey-mismatch sandbox=%s/%s err=%v", namespace, sandbox, err)
				return err
			}
			if first {
				log.Printf("audit hostkey-pinned sandbox=%s/%s fingerprint=%s", namespace, sandbox, ssh.FingerprintSHA256(key))
			}
			return nil
		},
		Timeout: 60 * time.Second,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, ccfg)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("upstream ssh: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	return client, func() { client.Close() }, nil
}

// proxyChannel opens the same channel type on the upstream connection and
// relays data, extended data and channel requests in both directions.
func proxyChannel(newCh ssh.NewChannel, up *ssh.Client) {
	chStart := time.Now()
	log.Printf("audit chan-open type=%s", newCh.ChannelType())
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
			if r.Type == "exec" || r.Type == "subsystem" {
				cmd := string(r.Payload)
				if len(cmd) > 140 {
					cmd = cmd[:140]
				}
				log.Printf("audit chan-req type=%s payload=%q", r.Type, cmd)
			} else if r.Type != "window-change" && r.Type != "env" {
				log.Printf("audit chan-req type=%s", r.Type)
			}
			ok, err := out.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
		}
	}
	var upstreamSide sync.WaitGroup // everything that flows sandbox -> user
	upstreamSide.Add(3)
	go relay(reqs, upCh, func() {})
	go relay(upReqs, ch, upstreamSide.Done)
	go func() { defer upstreamSide.Done(); io.Copy(ch, upCh); ch.CloseWrite() }()
	go func() { defer upstreamSide.Done(); io.Copy(ch.Stderr(), upCh.Stderr()) }()
	go func() { io.Copy(upCh, ch); upCh.CloseWrite() }()
	upstreamSide.Wait()
	log.Printf("audit chan-close type=%s after=%s", newCh.ChannelType(), time.Since(chStart).Round(time.Millisecond))
	ch.Close()
	upCh.Close()
}
