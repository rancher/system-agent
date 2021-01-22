# Configuration Example

Assuming a folder `/etc/rancher/agent`

Create a file called `config.yaml` in `/etc/rancher/agent` with the contents like:

```
workDirectory: /etc/rancher/agent/work
localPlanDirectory: /etc/rancher/agent/plans
remoteEnabled: true
connectionInfoFile: /etc/rancher/agent/conninfo.yaml
```

Create a file called `conninfo.yaml` in `/etc/rancher/agent` with the contents like:
```
kubeConfig: /etc/rancher/agent/kubeconfig
namespace: mynamespace
secretName: mysecret
```

Create a file called `kubeconfig` in `/etc/rancher/agent` with your kubeconfig

Ready to test? Create a secret like:

```
apiVersion: v1
kind: Secret
metadata:
  name: mysecret
  namespace: mynamespace
type: Opaque
data:
  plan: eyJpbnN0cnVjdGlvbnMiOlt7Im5hbWUiOiJpbnN0YWxsIiwiaW1hZ2UiOiJkb2NrZXIuaW8vb2F0czg3L2xvbHRnejppbnN0YWxsLXJrZTIiLCJjb21tYW5kIjoic2giLCAiYXJncyI6WyItYyIsImluc3RhbGwuc2giXX1dfQ==
```

The above secret is going to install RKE2.