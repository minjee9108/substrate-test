package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// routerDialer opens CONNECT tunnels to actors through atenet-router's
// CONNECT-TLS listener, using the pod's own podidentity client certificate.
// The router checks that certificate's SPIFFE ID against its allowlist, so the
// gateway's ServiceAccount is the only identity that needs to be listed.
type routerDialer struct {
	addr       string // host:port of the CONNECT-TLS listener
	serverName string
	certFile   string // credential-bundle.pem (key + chain), refreshed by kubelet
	tlsBase    *tls.Config
}

func newRouterDialer(addr, serverName, caFile, credFile string) (*routerDialer, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificates", caFile)
	}
	d := &routerDialer{addr: addr, serverName: serverName, certFile: credFile}
	d.tlsBase = &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverName,
		// Re-read on every handshake: the kubelet rotates the bundle in place.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			b, err := os.ReadFile(credFile)
			if err != nil {
				return nil, err
			}
			c, err := tls.X509KeyPair(b, b)
			if err != nil {
				return nil, fmt.Errorf("loading %s: %w", credFile, err)
			}
			return &c, nil
		},
	}
	if _, err := d.tlsBase.GetClientCertificate(nil); err != nil {
		return nil, err
	}
	return d, nil
}

// dial returns a tunnel to port on the actor "<atespace>/<name>".
func (d *routerDialer) dial(ctx context.Context, atespace, name, port string) (net.Conn, error) {
	var nd net.Dialer
	raw, err := nd.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to router: %w", err)
	}
	tc := tls.Client(raw, d.tlsBase.Clone())
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("TLS handshake with router: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { tc.Close() })
	defer stop()

	dest := net.JoinHostPort(name, port)
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: dest},
		Host:   dest,
		Header: http.Header{"Ate-Target-Actor": []string{atespace + "/" + name}},
	}
	if err := req.Write(tc); err != nil {
		tc.Close()
		return nil, fmt.Errorf("writing CONNECT: %w", err)
	}
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		tc.Close()
		return nil, fmt.Errorf("reading CONNECT response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		tc.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("router rejected CONNECT to %s: %s", dest, msg)
	}
	if !stop() {
		tc.Close()
		return nil, ctx.Err()
	}
	return &bufConn{Conn: tc, r: br}, nil
}

// bufConn keeps bytes http.ReadResponse buffered past the response header.
type bufConn struct {
	net.Conn
	r    *bufio.Reader
	once sync.Once
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
