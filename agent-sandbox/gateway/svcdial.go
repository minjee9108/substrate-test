package main

import (
	"context"
	"fmt"
	"net"
	"time"
)

// svcDialer reaches a sandbox through its headless Service. A sandbox that
// cannot be reached is resumed if it is suspended, and the dial is retried.
type svcDialer struct {
	kube      *kubeClient
	readyWait time.Duration
}

func (d svcDialer) dial(ctx context.Context, namespace, name, port string) (net.Conn, error) {
	addr := net.JoinHostPort(fmt.Sprintf("%s.%s.svc", name, namespace), port)
	var nd net.Dialer
	if c, err := nd.DialContext(ctx, "tcp", addr); err == nil {
		return c, nil
	}
	rctx, cancel := context.WithTimeout(ctx, d.readyWait)
	defer cancel()
	if err := d.kube.ensureRunning(rctx, namespace, name); err != nil {
		return nil, err
	}
	// Ready means the pod passed its probe, so the Service endpoint exists. DNS may lag by a moment.
	var last error
	for i := 0; i < 10; i++ {
		c, err := nd.DialContext(ctx, "tcp", addr)
		if err == nil {
			return c, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, last
}
