# POC report: a virtual desktop for business users on Substrate

- Date: 2026-11-19
- Environment: local `kind` cluster on Docker Desktop (arm64), gVisor sandbox
- Desktop under test: LinuxServer `webtop` (XFCE, streamed by Selkies over WebSocket)
- Builds on: [cloud_run_report.md](cloud_run_report.md) (SSH gateway, router, access policies)
- Status: proof of concept. Nothing is in the Substrate repo.

---

## 1. Summary

- The requirement: business users who want a graphical desktop, reachable only by the person who owns the actor.
- A webtop desktop runs in a Substrate gVisor actor and shows in a normal browser. It needed four image and template fixes (section 4).
- The desktop survives suspend and resume. Open windows and unsaved text came back. Suspend took about 5s and resume about 1s.
- An HTTP gateway now sits in front of the desktop. It reuses the SSH gateway's router dialer and ownership rule.
  - Host name: `<actor>--<atespace>.<domain>`.
  - A global owner, atespace owner or atespace editor gets in. A viewer, a stranger and another atespace get 403.
  - WebSocket works. The desktop has no login of its own.
- A revoked user is cut off in about 60 seconds, including a stream that is already open.
- Sign-in is a signed test token, not Entra. Entra OIDC is not built.
- Not done: closing the router bypass, auto-resume of a suspended desktop, the snapshot size, and clipboard, audio and file transfer.

---

## 2. What we tried

- **Phase 0. Desktop in an actor, reached over SSH.**
  - A template with two containers: the existing `ssh` image and the `desktop` image.
  - Port 3000 of the desktop was reached with `ssh -L` through the existing SSH gateway (`direct-tcpip`). No router change.
- **Phase 1. Suspend and resume.**
  - With a text editor holding unsaved text and a terminal open, we suspended and resumed the actor.
- **Phase 2. Owner-only HTTP gateway.**
  - A second listener in the gateway binary (`gateway/httpgw.go`, `gateway/token.go`).
  - A signed test token (Ed25519 JWT) replaces the Entra token for now.
  - A session cookie is set by `/_gw/login?token=...`. The token and cookie are removed before the request reaches the actor.
  - Every request is authorized with the same cached access-policy check as SSH.
  - Every tunnel is tracked. A sweep every 30s closes tunnels whose token expired or whose owner lost access.
- **Not run:** Phase 3 (business-user realism: audio, clipboard, latency) and Phase 4 (multi-user).

---

## 3. Results

| Check | Result |
|---|---|
| Desktop loads in a browser through the HTTP gateway, WebSocket included | Works |
| No token | 401 |
| Owner (`alice`, editor) | 200 |
| Viewer (`bob`) | 403 |
| Owner on another atespace | 403 |
| Suspend / resume | About 5s / about 1s. Windows and unsaved text returned |
| Revoke `alice` (editor to viewer) | Open tunnel closed after 59s. New requests got 403 after 60s |
| Golden snapshot build | About 35 to 47s |
| Resume from the golden snapshot | About 1s |
| Idle for 6+ minutes through the router | Works after the keepalive fix (section 5) |

The 60s revocation time is the 30s policy cache plus the 30s sweep.

---

## 4. What the desktop needed in gVisor

- **Capabilities.** The `desktop` container needs `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SYS_CHROOT`. Without them s6-overlay and nginx fail with `Operation not permitted` and the container crash-loops.
- **A shim for `bwrap`.**
  - Symptom: a black desktop with no panel.
  - Cause: gVisor cannot set up a loopback device in a new network namespace, so `bwrap --unshare-all` fails. The image loader (glycin) uses `bwrap`, so icon loading fails and `xfce4-panel` aborts.
  - Fix: `desktop/image/bwrap-shim.sh` adds `--share-net` after `--unshare-all`.
- **Text size.**
  - Symptom: oversized, unreadable text. Selkies takes the DPI from the browser, which was 288 on a HiDPI laptop.
  - Fix: `SELKIES_SCALING_DPI=96` with a fixed `SELKIES_MANUAL_WIDTH/HEIGHT` of 1920x1080.
- **Egress for browsing.**
  - With no egress policy the actor has no DNS, so the in-desktop browser cannot resolve names.
  - A hostname list of Google domains works for a narrow test.
  - Hostname `"*"` on port 443 with TLS passthrough allows all HTTPS. This gives up hostname control.
  - UDP, QUIC, WebRTC and plain HTTP on port 80 stay blocked.

Files: `desktop/image/`, `desktop/k8s/template-desktop-v3.yaml`, `desktop/policies/`.

---

## 5. Problems found and what we did

- **The router drops a tunnel after 5 minutes of idle.**
  - The SSH keepalives from the client end at the gateway and are not forwarded to the actor.
  - Fix: the gateway sends an SSH keepalive to the actor every 60s (`-upstream-keepalive`). This also helps Claude Desktop sessions that sit quiet. Verified with a 6+ minute idle session.
- **A recreated actor name trips the host-key pin.** The gateway pins each actor's host key. A new actor with an old name is refused. We used new names. The gateway image has no shell, so the pin file needs another way to edit. This is a gap.
- **`ssh -L` tunnels are fragile.** They die on suspend, laptop sleep and certificate expiry, and a browser tab drops on suspend. The HTTP gateway removes the tunnels, but it does not yet re-dial or resume a suspended actor.
- **The local disk filled up.** A 3.34GB image existed in four places on a full disk. The Docker VM went read-only and etcd failed on both clusters. State survived a restart. Use a smaller desktop image.

---

## 6. Limitations

- **No Entra sign-in.** The token is signed by a key we made. A real deployment needs the OIDC flow and a mapping from an Entra subject to the Substrate principal.
- **The router still accepts `atenet-client`.** Until it is removed from the router allowlist, `kubectl ate proxy` reaches the desktop without the gateway. The desktop has no login of its own.
- **No auto-resume.** A request for a suspended desktop returns 502.
- **The session cookie is not `Secure`.** The POC uses plain HTTP on localhost. It needs TLS and a real domain.
- **One tunnel per HTTP request.** This keeps one user's connection from carrying another's request. Many connections may cost latency.
- **Allow-all HTTPS egress is wide.** Business browsing needs a decision on which hosts to allow.
- **The image is 3.34GB.** Snapshot size and restore time were not measured.
- **Clipboard, audio, file transfer, latency and several users were not tested.**
- **Local `kind` only.** Work stops when the laptop sleeps.

---

## 7. Next steps

1. Auto-resume a suspended desktop on request. The gateway needs `editor` on the atespace to call resume.
2. Remove `atenet-client` from the router allowlist in a test install and confirm direct access is rejected.
3. Entra OIDC sign-in, then the principal mapping.
4. Measure snapshot size, and test clipboard, audio, file transfer and latency.
5. Try a smaller desktop image.

---

## 8. Files

- `gateway/httpgw.go`, `gateway/token.go`, `gateway/httpgw_test.go`: the HTTP gateway, the test token, and tests.
- `gateway/main.go`: new flags (`-http-listen`, `-http-domain`, `-http-actor-port`, `-token-pub`, `-upstream-keepalive`) and `issue-token`.
- `gateway/ssh-gateway.yaml`: the HTTP port, Service port and the token ConfigMap mount.
- `desktop/`: the image, the pool and template, the egress policies, and `wait-template.sh`.

To try it, issue a token and open the login link:

    go run ./gateway issue-token -key token_key -sub alice -ttl 8h
    kubectl -n ate-ssh-gateway create configmap http-gateway-token-pub --from-file=token_pub=token_key.pub
    kubectl -n ate-ssh-gateway port-forward svc/ssh-gateway 8080:80
    open "http://<actor>--<atespace>.localhost:8080/_gw/login?token=<token>"
