# agent-sandbox SSH POC

Claude Code Desktop SSH sessions against a `Sandbox` from kubernetes-sigs/agent-sandbox,
behind an SSH gateway. Same idea as the Substrate POC, on plain Kubernetes.

```
ssh / Claude Desktop  --SSH #1 (cert)-->  ssh-gateway (pod)  --SSH #2 (gateway key)-->  Sandbox pod (sshd)
                                              |  RBAC check (SubjectAccessReview)
                                              |  resume if suspended (patch operatingMode)
                                              +-> kube-apiserver
```

- `image/` actor image: sshd + Claude Code, home on a PVC, host key on its own PVC.
- `gateway/` Go gateway. Target is the SSH user `<sandbox>.<namespace>`.
- `k8s/` Sandbox, gateway Deployment and RBAC, per-namespace Role and RoleBinding.
- `keys/` generated keys (git-ignored). `gw-tool issue` signs certificates.

## Rebuild
```sh
kind create cluster --name agent-sandbox
kubectl --context kind-agent-sandbox apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.10/sandbox-with-extensions.yaml
docker build -t ssh-sandbox:v1 image && kind load docker-image ssh-sandbox:v1 --name agent-sandbox
(cd gateway && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o image/ssh-gateway . && go build -o ../gw-tool .)
docker build -t ssh-gateway:v2 gateway/image && kind load docker-image ssh-gateway:v2 --name agent-sandbox
# keys: gateway_upstream, gateway_host, ca_key, id_alice (ssh-keygen -t ed25519 -N '' -f keys/<name>)
kubectl --context kind-agent-sandbox create ns team-a
kubectl --context kind-agent-sandbox -n team-a create secret generic ssh-authorized-keys --from-file=authorized_keys=keys/gateway_upstream.pub
kubectl --context kind-agent-sandbox apply -f k8s/sandbox.yaml -f k8s/gateway.yaml -f k8s/rbac-team-a.yaml
kubectl --context kind-agent-sandbox -n sandbox-gateway create secret generic ssh-gateway-keys --from-file=host_key=keys/gateway_host --from-file=upstream_key=keys/gateway_upstream
kubectl --context kind-agent-sandbox -n sandbox-gateway create configmap ssh-gateway-ca --from-file=ca_key.pub=keys/ca_key.pub
```

## Connect
```sh
kubectl --context kind-agent-sandbox -n sandbox-gateway port-forward svc/ssh-gateway 2226:22
./gw-tool issue -ca keys/ca_key -pub keys/id_alice.pub -principal alice -ttl 4h | tail -1 > keys/id_alice-cert.pub
ssh -p 2226 -i keys/id_alice -o CertificateFile=keys/id_alice-cert.pub ssh-1.team-a@127.0.0.1
```
```
# ~/.ssh/config for Claude Desktop
Host sandbox-1
  HostName 127.0.0.1
  Port 2226
  User ssh-1.team-a
  IdentityFile ~/agent-sandbox-poc/keys/id_alice
  CertificateFile ~/agent-sandbox-poc/keys/id_alice-cert.pub
  IdentitiesOnly yes
  StrictHostKeyChecking accept-new
```

## Access model
A user may connect to every Sandbox in a namespace if RBAC lets them `update` Sandboxes there.
Add a user with a RoleBinding to the `sandbox-editor` Role. The certificate principal is the RBAC user name.
