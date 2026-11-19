# Substrate and agent-sandbox for remote agent environments

- Date: 2026-11-12
- Both were tested as the remote machine for Claude Code Desktop SSH sessions, behind the same style of SSH gateway.
- Both ran on a local `kind` cluster on Docker Desktop (arm64).
- Details are in `substrate.md` and `agent-sandbox.md`.
- Key to the **Source** column: **Tested** means we ran it. **Code** means we read it in the repository or docs. **Inferred** means we did not verify it.

---

## 1. What each one is

| | Substrate | agent-sandbox |
|---|---|---|
| Project | [agent-substrate/substrate](https://github.com/agent-substrate/substrate) | [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox), a SIG Apps project |
| Version tested | Built from the main branch, installed 2 Nov 2026 | v1.0.10 release |
| Unit of work | Actor, scheduled onto a shared worker pod | `Sandbox`, one pod per sandbox |
| Core idea | Many mostly idle actors share few ready workers. Suspend and resume saves and restores them. | One stateful pod with a stable name and volumes. The controller runs it. |
| Sandboxing | Built in. Actors run only in gVisor or a microVM. | Not provided. The pod uses a `RuntimeClass` you supply, such as gVisor or Kata. |
| Compatibility promise | None before v1.0.0 (the repo's own rule) | Not checked. The release is v1.0.10, and the Sandbox API version we used is `v1beta1`. |

---

## 2. Lifecycle and state

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Suspend | Writes a snapshot of memory and the root filesystem to object storage | Core controller: deletes the pod and keeps the volumes. With GKE Pod Snapshots: takes a snapshot first, then deletes the pod. See section 3. | Substrate: tested. Sandbox core: tested. GKE snapshots: Code, not tried. |
| What survives suspend | Running processes (memory fidelity), files, open state | Core controller: files on volumes only. With GKE Pod Snapshots: memory and filesystem. | Substrate: tested. Sandbox core: tested. GKE snapshots: Code, not tried. |
| A running task across suspend | Survives. Verified: a Claude task kept running after Desktop closed and a laptop sleep. | Core controller: lost. A background process did not survive. With GKE Pod Snapshots: restored, by the docs. | Substrate: your test. Sandbox core: tested. GKE snapshots: Code, not tried. |
| Resume | Restores from the snapshot onto any free worker | Creates a new pod and reattaches volumes. With GKE Pod Snapshots the new pod also restores from the latest snapshot. | Substrate: tested. Sandbox core: tested. GKE snapshots: Code. |
| Automatic idle suspend | None in the build we tested | None. A sandbox can expire at a set time with `shutdownTime`. | Code both |
| Warm start | Golden snapshots and tags | `SandboxWarmPool` of pre-created sandboxes. With GKE Pod Snapshots, pool members can boot from one golden snapshot. | Code both |
| Changing the workload definition | A template is immutable. A change needs a new template version. | A pod change applies only when the pod is recreated. | Tested both |
| Storage of state | In-cluster `rustfs` on kind. Object storage on a real cluster. | PersistentVolumeClaims | Tested both |
| Multiplexing | Many actors share a small worker pool | One pod per sandbox while it runs | Code both |

---

## 2A. Snapshots

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Snapshot support | Built in | Not in the core controller. Added by GKE Pod Snapshots (`podsnapshot.gke.io`), which the project's Python SDK drives. | Code both |
| What a snapshot holds | Process memory and root filesystem changes | Memory and filesystem, by the project's docs | Code both |
| Object storage backend | GCS (the default) or S3. On `kind` it is the in-cluster `rustfs`. | GCS in every example and doc we read. A `PodSnapshotStorageConfig` points at a bucket. We did not look for other backends. | Code both |
| Who runs the snapshot | Substrate's own components, with a storage plugin | GKE's snapshot controller, outside agent-sandbox | Code both |
| Where it can run | Any cluster that meets the kernel need. Tested on `kind`. | GKE Autopilot, or GKE Standard 1.35.3 or newer, with a gVisor node pool and snapshots enabled | Code both |
| Runtime requirement | gVisor or microVM | gVisor | Code both |
| Access to the bucket on GCS | `ate-api-server` hands each actor a short-lived token limited to that actor's prefix. Atelet has no direct access. | The pod's Kubernetes ServiceAccount gets `objectUser` on the bucket through Workload Identity. GKE's service agent also needs `objectUser` to delete snapshots. | Code both |
| Restore scope | A golden snapshot or tag restores into new actors. An actor restores from its own snapshot. | By default a sandbox restores only from its own snapshots. A shared policy lets pool members restore from one golden snapshot. | Code both |
| Trigger | Suspend writes the snapshot. | The Python SDK creates a `PodSnapshotManualTrigger`. `suspend(snapshot_before_suspend=True)` snapshots, then suspends. | Code both |
| SDK coverage | `kubectl ate` | Python only. The Go SDK does not wrap it yet. | Code |
| Tried here | Yes | No | |

- Both can store snapshots in GCS. The difference is who owns the feature.
- On Substrate it is part of the platform. On agent-sandbox it depends on GKE.
- agent-sandbox on a plain cluster, such as `kind`, has no snapshots. That is what we tested.

---

## 3. Isolation and capabilities

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Container isolation | gVisor (tested) or Cloud Hypervisor microVM | Default `runc` in our test. gVisor or Kata through a `RuntimeClass`. | Tested both |
| What `sshd` needed | Six capabilities added in the template (`SYS_CHROOT`, `SETUID`, `SETGID`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`), on top of Substrate's default set (`AUDIT_WRITE`, `KILL`, `NET_BIND_SERVICE`) | The same six plus `AUDIT_WRITE`, because the manifest used `drop: [ALL]` and so removed the default | Tested both. The Substrate default set is from the API guide. |
| Terminal (pty) requests | Worked. `AUDIT_WRITE` is in the default set and we did not drop it. | Failed until `AUDIT_WRITE` was added back | Tested both |
| Why the difference | Not a runtime difference. Substrate's default capability set already includes `AUDIT_WRITE`. | The manifest dropped all capabilities, and Debian's OpenSSH ends the session when its audit write fails with `EPERM` | Sandbox: tested. Substrate: Code. |
| Host requirement | Docker Desktop kernel 6.8 or newer, for the gVisor overlay snapshot | None beyond a normal Kubernetes cluster | Tested |
| Service account token in the actor | Not mounted by default | `automountServiceAccountToken: false` set by us | Code both |

---

## 4. Networking and egress

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| How a client reaches the workload | Through `atenet-router` CONNECT-TLS, with a SPIFFE client certificate | Plain TCP to the sandbox's headless Service | Tested both |
| What the gateway needed for that | A router client with a pod certificate, and a WebSocket bridge in the actor | A TCP dial | Tested both |
| Who may reach actors | The router allows only listed SPIFFE IDs. We listed only the gateway. | Any pod in the cluster can reach the Service. We did not restrict it. | Tested both |
| Outbound traffic default | Blocked. DNS for unlisted names returns `NXDOMAIN`. | Open | Tested both |
| Outbound control | Per-actor policy by hostname, applied within about 5 seconds | Kubernetes `NetworkPolicy` filters by IP. Hostname filtering needs a plugin such as Cilium. | Substrate: tested. Sandbox: Inferred, the project has a Cilium example. |
| Outbound path | Through a shared egress gateway with per-actor identity | Directly from the pod | Tested both |
| Credential injection | The egress gateway can replace a placeholder header with a secret. It needs an `https` rule. | None | Substrate: Code. Sandbox: none found. |
| Long-run egress failure | Yes. Egress dropped for up to about 54 minutes after a laptop sleep. | Not applicable. There is no per-actor certificate. | Substrate: tested and traced in code |

---

## 5. Access control and identity

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Authorization system | Embedded OpenFGA model with roles in access policies | Kubernetes RBAC | Code both |
| Scope of a grant | Per atespace, and a global scope | Per namespace, or the whole cluster | Tested both |
| Rule for "may connect" | Global owner, atespace owner, or atespace editor | RBAC allows `update` on Sandboxes in the namespace | Tested both |
| How the gateway checks | Reads two access policies over gRPC and applies a copy of the model | One `SubjectAccessReview` call. Kubernetes answers. | Tested both |
| Risk in that check | The gateway's copy of the rules can drift from the real model | None. The API server decides. | Code |
| Permission-check API | None. The gateway cannot ask "may X do Y". | `SubjectAccessReview` | Code both |
| Gateway's own rights | Global `viewer` in an access policy | `create` on `subjectaccessreviews`, and `get` and `patch` on Sandboxes cluster-wide | Tested both |
| User identity | Token `sub` claim, as `user:<principal>` | Kubernetes user name | Tested both |
| Tests of revoke | A live session ended after 31 seconds | A live session ended after 31 seconds | Tested both |
| Bootstrap owners | Set by server flags and invisible to the gateway | Not applicable | Code |

---

## 6. Secrets and snapshots

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| What a stored copy holds | A snapshot has full process memory and the disk changes, so it can hold tokens and transcripts | Without GKE Pod Snapshots, volumes hold files only. With them, a snapshot holds memory and filesystem, so the same exposure applies. | Code both |
| Encryption of stored state | None seen in code. Per-actor encryption is listed as future work. | Not examined. It depends on the storage class. | Substrate: Code. Sandbox: not checked. |
| Storage credentials | Per-actor tokens on GCS. One shared key on S3 and on `kind`. | With GKE snapshots, the pod's own identity can read and write the bucket. Without them, not applicable. | Code |
| Ways to keep a secret out of the workload | Egress credential injection, short-lived tokens | A mounted Secret or env var puts it in the pod | Code |
| Vault | A Vault provider for credential injection is possible but not in the tree | Not examined | Code |
| Deleting state | Deleting an actor removes its snapshots. Tag copies remain. | Deleting a Sandbox deletes its volumes. GKE removes snapshot objects on retention pruning or `kubectl delete podsnapshots`. | Code both |

---

## 7. Operating it

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Install | `ate-setup` builds and deploys many components: API server, controller, kubelet agent, router, egress, workers | One release manifest and one controller | Tested both |
| Time to a working workload | Long. It needed a kernel update, image builds, worker pools, and templates. | Short. About one session. | Tested both |
| Moving parts in the cluster | About ten pods plus object storage | One controller pod | Tested both |
| Gateway size | Larger. A router client, a gRPC client, and a WebSocket bridge in the actor. | Smaller. A Kubernetes client and a TCP dial. | Tested both |
| Image build | An extra bridge program in the image | `sshd` runs directly as PID 1 | Tested both |
| Resume on connect | Not built | Built. The gateway patches `operatingMode` and waits for Ready. | Tested sandbox |
| Observability | `kubectl ate logs` shows `sshd` and bridge lines. A metric registry exists. | `kubectl logs` shows `sshd` lines | Tested both |
| Laptop sleep | Breaks certificate renewal inside the VM | Breaks only the local port-forward | Tested both |
| Tooling | A custom `kubectl ate` plugin | Standard `kubectl` | Tested both |
| Real-cluster reach for SSH | A load balancer in front of the gateway | A load balancer in front of the gateway | Same for both |

---

## 8. Claude Code Desktop results

| | Substrate | agent-sandbox | Source |
|---|---|---|---|
| Connect and run a prompt | Yes, through the in-cluster gateway | Connected, deployed its helper, and reattached to a Claude session | Substrate: yes. Sandbox: partly. |
| Terminal and shell commands | Worked | Failed until `AUDIT_WRITE` was added to the container capabilities. The failure was `sshd` ending the session at Desktop's terminal request, because it could not write its audit record (see section 3). A terminal from the command line works after the fix. A retest in Desktop is pending. | Substrate: tested. Sandbox: tested from the command line, not yet in Desktop. |
| Quit Desktop, task keeps running | Yes. You verified this. | The process stayed alive. We did not see a task finish. | Substrate: your test. Sandbox: observed. |
| Reconnect after quitting | Yes | Reconnected and reattached to the helper | Tested both |
| SFTP | Yes | Yes | Tested both |
| Concurrent sessions | Six at once | Six at once | Tested both |
| Certificate expiry mid-session | Closed in 61 s with a 60 s certificate | Closed in 46 s with a 45 s certificate | Tested both |
| Survives a pod or actor restart | Yes, memory is restored | Core controller: no. The session process is lost and the transcripts remain. With GKE Pod Snapshots: expected yes, not tried. | Substrate: tested. Sandbox core: tested. |

---

## 9. Strengths and weaknesses

| | Substrate | agent-sandbox |
|---|---|---|
| Strengths | Saves and restores whole processes, so long tasks survive suspend. Built-in isolation. Default-deny egress with per-actor hostname rules. Credential injection at the egress gateway. Many idle actors share few workers. | Small and simple to install and run. Standard Kubernetes tools and RBAC. A tiny gateway with no router client. A release numbered v1.x from a SIG project, though the Sandbox API we used is `v1beta1`. Works on any conformant cluster. |
| Weaknesses | Heavy install and many components. Pre-1.0 with no compatibility promise. Needs a recent kernel. A long-run egress failure after VM pause, which we traced to timers on the wrong clock. Snapshots can hold secrets and have no encryption yet. No permission-check API. | The core suspend loses all running processes. Memory snapshots need GKE Pod Snapshots, which work only on GKE with gVisor, through the Python SDK. Open egress by default. No isolation until you add a runtime. One pod per sandbox while running. No credential injection. No idle suspend. |
| Hardest problem we hit | Egress dropping after a laptop sleep | Terminals failing from a missing capability |

---

## 10. When to choose which

| If you need | Choose | Why |
|---|---|---|
| Long unattended agent tasks that must survive a suspend | Substrate, or agent-sandbox on GKE with Pod Snapshots | Substrate restores process memory on any supported cluster. agent-sandbox does so only through GKE Pod Snapshots, which we did not try. |
| A fast, simple way to try the idea | agent-sandbox | One manifest and standard tools |
| Strong default isolation and a controlled network | Substrate | gVisor and per-actor egress policy come with it |
| To reuse existing Kubernetes RBAC and tooling | agent-sandbox | The access check is one API call |
| Many idle workloads on few machines | Substrate | Workers are shared across actors |
| A project with a versioned release and a Kubernetes SIG home | agent-sandbox | Substrate makes no compatibility promise before v1.0.0. We did not check agent-sandbox's own policy, and its Sandbox API is `v1beta1`. |
| To keep tokens out of the workload | Substrate | The egress gateway can inject them. agent-sandbox has no equivalent. |
| To run on a plain cluster with no special kernel | agent-sandbox, without snapshots | Substrate needed a new kernel for gVisor snapshots. agent-sandbox has no snapshots on a plain cluster. |

---

## 11. Gaps in each, if the other's strengths are wanted

| To get | On Substrate | On agent-sandbox |
|---|---|---|
| Persistence of running tasks | Already there | GKE Pod Snapshots, backed by a GCS bucket, save memory and filesystem. They need GKE with gVisor and the Python SDK. We did not try them. |
| Simple install and standard tooling | Not available | Already there |
| Hostname egress filtering | Already there | Add Cilium or a similar plugin |
| Sandbox isolation | Already there | Install gVisor or Kata and set a `RuntimeClass` |
| Credential injection | Already there. A Vault provider would need writing. | Not available. Use an LLM proxy or a sidecar, which we did not evaluate. |
| Idle suspend | Build a reaper. The suspend request API exists. | Build a reaper. A running task would be lost, so the rule must be safe. |
| Simple RBAC-based access | A custom authorization copy in the gateway | Already there |

---

## 12. What to test next to decide

- Run a long Claude task for over an hour in each, close Desktop, and compare the result.
- Restart the workload mid-task in each, and compare what is recovered.
- Test GKE Pod Snapshots with agent-sandbox on GKE, to see if memory-preserving suspend closes the gap. Include a Claude task running across a suspend and resume.
- Compare the bucket access model. Substrate scopes each actor to its own prefix on GCS. agent-sandbox gives the pod's identity access to the bucket.
- Add gVisor and a hostname egress filter to agent-sandbox, and repeat the Desktop tests.
- Move both to a cluster that does not sleep, and check whether Substrate's egress failure is only a laptop effect.
