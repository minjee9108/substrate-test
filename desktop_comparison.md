# Substrate and agent-sandbox for a business-user virtual desktop

- Date: 2026-11-19
- The same desktop (LinuxServer `webtop`, XFCE over Selkies) and the same owner-only HTTP gateway code were tried on both.
- Both ran on a local `kind` cluster on Docker Desktop (arm64).
- Details: [desktop_poc_report.md](desktop_poc_report.md) (Substrate) and [agent-sandbox/desktop_poc_report.md](agent-sandbox/desktop_poc_report.md).
- The SSH comparison is in [agent-sandbox/ssh_comparison.md](agent-sandbox/ssh_comparison.md).
- Key to the **Source** column: **Tested** means we ran it. **Code** means we read it in the repository or docs. **Inferred** means we did not verify it.

---

## 1. Running the desktop

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Runtime | gVisor (the only option besides a microVM) | runc (default). A `RuntimeClass` such as gVisor is possible but was not tried. | Tested |
| Stock webtop image works | No. Needed a `bwrap` shim, so the image was rebuilt. | Yes, unchanged | Tested |
| Extra capabilities | `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SYS_CHROOT` | None | Tested |
| Time to a ready desktop | Golden snapshot build about 35 to 47s, then resume about 1s | Pod ready in about 15s | Tested |
| Text size fix | `SELKIES_SCALING_DPI=96` and fixed resolution | The same settings | Tested |
| Isolation of the desktop | gVisor sandbox, with the actor's own network namespace | Shared node kernel, as with any runc pod | Code |

---

## 2. Suspend and resume

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Suspend time | About 5s | 64s until the pod was gone (cause not investigated) | Tested |
| Resume time | About 1s | About 11s, from the first request to Ready | Tested |
| Open windows and unsaved text | Came back (confirmed in the browser) | Lost (process check: the editor and terminal were gone) | Tested |
| Files | Survive | Survive on the volume | Tested |
| Why | Memory and filesystem snapshot to object storage | The controller deletes the pod and keeps the PVC. A memory snapshot exists only as GKE Pod Snapshots. | Substrate: tested. GKE snapshots: Code, not tried. |
| Resume when a user opens the URL | Not built. The gateway returns 502. | Built. The gateway patches `operatingMode` and waits for Ready. | Tested |
| Snapshot size | Not measured | Not applicable | |

---

## 3. Reaching the desktop and who may

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Path from the gateway | CONNECT tunnel through `atenet-router` | Plain TCP to the headless Service | Tested |
| Idle cut | The router drops an idle tunnel after 5 minutes. The gateway sends an upstream keepalive every 60s to avoid it. | No router. Not tested for a long idle session. | Substrate: tested. Sandbox: Inferred |
| Who the router or network lets in | Only SPIFFE identities on the router allowlist. `atenet-client` is still listed, so `kubectl ate proxy` is a bypass until it is removed. | Anything that can reach the Service in the cluster. A `NetworkPolicy` is needed, and kind does not enforce it. | Tested |
| Ownership rule | Global owner, atespace owner or atespace editor, read from Substrate access policies | May the user `update` Sandboxes in the namespace, by Kubernetes RBAC | Tested |
| Per-instance ownership | Atespace is the unit | Namespace is the unit. One Sandbox only with a `resourceNames` rule. | Substrate: tested. Sandbox: Code |
| Gateway identity toward the API | Own SPIFFE certificate and service account token | Own service account, with `create` on `subjectaccessreviews` and `get`/`patch` on Sandboxes | Tested |
| Revocation | Open tunnel closed after 59s, new requests denied after 60s | Open tunnel closed after 18s. Worst case about 60s. | Tested |
| Sign-in | Signed test token. Entra not built. | Same token. The subject must equal the RBAC user name. | Tested |
| Gateway code | `httpgw.go`, `token.go` | The same files, plus `svcdial.go` | Tested |

---

## 4. Network egress for browsing

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Default | No DNS or outbound until a policy is set | Open | Tested |
| Allow some HTTPS hosts | Per-actor policy with host names and wildcards | Not with a plain `NetworkPolicy` (it matches IP ranges). Needs an egress proxy or a CNI with DNS rules. | Substrate: tested. Sandbox: Code |
| Allow all HTTPS | Host name `"*"` on port 443 | Default | Tested |
| UDP, QUIC, WebRTC, port 80 | Blocked | Open | Tested |

---

## 5. What the evaluation changes

- **If the desktop must keep open windows across suspend**, only Substrate does it on a laptop. agent-sandbox needs GKE Pod Snapshots, which we could not test.
- **If the desktop must start without engineering around the sandbox**, agent-sandbox wins on runc. Substrate needs image and template fixes because gVisor lacks features the desktop expects. A gVisor `RuntimeClass` on agent-sandbox may bring the same fixes. We did not test this.
- **If business users browse the web**, Substrate gives hostname control without extra parts. agent-sandbox needs more infrastructure for the same control.
- **Operations differ.** Substrate has a router and an idle limit to manage, and it needs `atenet-client` removed. agent-sandbox has fewer parts, and the Service must be protected by network policy.
- **Resume on request was easier on agent-sandbox** because the controller already exposes `operatingMode`. On Substrate it needs the gateway to call `ate-api` with `editor` on the atespace.
- **Neither has Entra sign-in yet.** Both would use the same gateway code with a different subject mapping.
