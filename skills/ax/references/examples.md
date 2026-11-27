# Example manifests

Adapted from `hack/tour.sh`, which runs them end to end. Apply with
`ax apply -f file.yaml` or `ax apply -f - <<EOF`. Replace the image with your
own digest-pinned agent image.

## Agent on a cloned repo

A Gateway that reaches only GitHub, a Workspace that clones a repo once and
snapshots it, and a Task that starts warm from that snapshot.

```yaml
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: github
spec:
  egress:
    - https:
        hostnames: ["github.com", "*.github.com", "*.githubusercontent.com"]
---
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: chalk
spec:
  profile:
    image: "ghcr.io/google/ax/ax-runner@sha256:0937ebe5ad2d8db6bb25e88ded44d2d65de1b1bd8f0a9a97c6bb3787407d9947"
  setup:
    gateway:
      name: github              # egress for setup only
    git:
      - repo: "https://github.com/chalk/chalk.git"
        depth: 1
    files:
      - path: AGENTS.md
        content: |
          Run `npm test` before submitting changes.
    commands:
      - "git -C chalk log -1 --format='%h %s' > setup.txt"
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: survey
spec:
  workspace:
    name: chalk                 # omit spec.profile to use the Workspace's
  gateway:
    name: github
  goal:
    text: "Survey chalk's JavaScript sources."
  config:                       # free-form; read from $AX_CONFIG_FILE
    dir: chalk/source
    pattern: "*.js"
  commands: ["sh", "-c", "cat setup.txt; sleep 3600"]
  debug: true
```

```bash
ax resume task survey
ax ssh survey -- ls -A /workspace
```

## Injecting an API key

The Gateway replaces the header outside the sandbox; the task holds only a
placeholder. The Secret's namespace must be granted to the atespace in
`k8s-credential-provider-namespace-policy` (`docs/manifests.md`).

```bash
kubectl create secret generic gemini --from-literal=api-key="$GEMINI_API_KEY"
kubectl label secret gemini ax.io/credential=true
```

```yaml
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: gemini
spec:
  egress:
    - https:
        hostnames: ["generativelanguage.googleapis.com"]
        effects:
          replaceHeaders:
            - header: x-goog-api-key
              credentialUri: "ate-secret://k8s.io/default/default/gemini/api-key"
    - https:
        hostnames: ["pypi.org", "files.pythonhosted.org"]
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ask-gemini
spec:
  profile:
    image: "ghcr.io/my-org/gemini-agent@sha256:..."
    resources:
      limits: {cpu: "2", memory: "4Gi"}
  gateway:
    name: gemini
  env:
    - name: GEMINI_API_KEY
      value: placeholder        # replaced by the Gateway
  commands: ["python3", "agent.py"]
```

## Model

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
spec:
  provider: google              # the only provider for now
  model: gemini-3.8-flash
  secretKey:
    name: gemini-api-secret
    key: GEMINI_API_KEY
```
