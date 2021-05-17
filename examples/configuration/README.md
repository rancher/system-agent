# Configuration Example

Assuming a folder `/etc/rancher/agent`

Create a file called `config.yaml` in `/etc/rancher/agent` with the contents like:

```
workDirectory: /var/lib/rancher/agent/work
localPlanDirectory: /var/lib/rancher/agent/plans
remoteEnabled: true
connectionInfoFile: /etc/rancher/agent/conninfo.yaml
preserveWorkDirectory: true
```

Create a file called `conninfo.yaml` in `/etc/rancher/agent` with the contents like:
```
kubeConfig: |-
  kubeConfig: |-
    apiVersion: v1
    kind: Config
    clusters:
    - name: "kubernetes"
      cluster:
        server: "https://my-k8s-apiserver"
    users:
    - name: "cluster-admin"
      user:
        token: <redacted>
    contexts:
    - name: "kubernetes"
      context:
        user: "cluster-admin"
        cluster: "kubernetes"
    current-context: "kubernetes"
namespace: mynamespace
secretName: mysecret
```

Ready to test? Create a secret like:

```
apiVersion: v1
kind: Secret
metadata:
  name: mysecret
  namespace: mynamespace
type: Opaque
data:
  plan: e2luc3RydWN0aW9uczpbbmFtZTppbnN0YWxsLWszc119IHtpbnN0cnVjdGlvbnM6W2ltYWdlOmRvY2tlci5pby9yYW5jaGVyL3N5c3RlbS1hZ2VudC1pbnN0YWxsZXItazNzOnYxLjIxLjAtazNzMV19Cg==
```

The above secret is going to install K3s.