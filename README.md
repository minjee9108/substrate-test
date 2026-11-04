# substrate-test

Proof of concept: use a [Substrate](https://github.com/agent-substrate/substrate) actor as the remote
environment for Claude Code Desktop SSH sessions, behind a central SSH gateway.

Read [cloud_run_report.md](cloud_run_report.md) first. It covers the architecture, what we tried,
the issues, and the limitations.

## Layout
- `spike/` is the one-user path: actor image (`sshd` and a WebSocket bridge), template, worker pool,
  client proxy, and a `ProxyCommand` script.
- `gateway/` is the SSH gateway (Go): certificate login, access-policy authorization, host-key pinning,
  channel proxy, in-cluster router and `ate-api` clients, tests, Dockerfile, and Kubernetes manifest.
- `gateway/ateapi/` is a copy of the generated `ate-api` Go package from the Substrate repo
  (`api/ateapi/v1`, Apache-2.0, Google LLC). It is here only so the gateway builds on its own.
- `policies/` holds the egress and global access policies we used.

## Not included
- Private keys, the SSH CA key, host keys, pin files, logs, and binaries. Generate your own:
  `ssh-keygen -t ed25519` for the gateway host key, the upstream key (public half goes in
  `spike/bridge/authorized_keys`), and the CA key.
- `spike/bridge/authorized_keys` is a placeholder for your gateway upstream public key.

## Status
Prototype. Not production ready. See the limitations in the report.
