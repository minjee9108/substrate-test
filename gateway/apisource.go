package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	ateapi "sshgateway/ateapi"
)

// grpcSource reads access policies from ate-api over gRPC, acting as the
// gateway's own identity (its ServiceAccount token). That identity needs
// global viewer and nothing more.
type grpcSource struct{ client ateapi.ControlClient }

func (g grpcSource) fetch(ctx context.Context, atespace string) (policy, error) {
	var (
		ap  *ateapi.AccessPolicy
		err error
	)
	if atespace == "" {
		ap, err = g.client.GetGlobalAccessPolicy(ctx, &ateapi.GetGlobalAccessPolicyRequest{Name: "default"})
	} else {
		ap, err = g.client.GetAtespaceAccessPolicy(ctx, &ateapi.GetAtespaceAccessPolicyRequest{
			Atespace: &ateapi.ObjectRef{Name: atespace},
			Name:     "default",
		})
	}
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// No policy for this scope is a policy with no bindings.
			return policy{bindings: map[string]map[string]bool{}}, nil
		}
		return policy{}, fmt.Errorf("reading access policy (%s): %w", scopeName(atespace), err)
	}
	p := policy{bindings: map[string]map[string]bool{}}
	for _, b := range ap.GetBindings() {
		if p.bindings[b.GetRole()] == nil {
			p.bindings[b.GetRole()] = map[string]bool{}
		}
		for _, m := range b.GetMembers() {
			p.bindings[b.GetRole()][m] = true
		}
	}
	return p, nil
}

// tokenFileCreds sends the contents of a (kubelet-refreshed) ServiceAccount
// token file as the bearer token on every call.
type tokenFileCreds string

func (c tokenFileCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(string(c))
	if err != nil {
		return nil, fmt.Errorf("reading token: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}
func (tokenFileCreds) RequireTransportSecurity() bool { return true }

func newGRPCSource(addr, caFile, serverName, tokenFile string) (grpcSource, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return grpcSource{}, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return grpcSource{}, fmt.Errorf("%s holds no certificates", caFile)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: serverName,
		})),
		grpc.WithPerRPCCredentials(tokenFileCreds(tokenFile)),
	)
	if err != nil {
		return grpcSource{}, err
	}
	return grpcSource{client: ateapi.NewControlClient(conn)}, nil
}
