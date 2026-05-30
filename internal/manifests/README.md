# AX Harness Deployment on Kubernetes

This directory contains manifests to deploy AX with the `harness` configuration path on Kubernetes using Agent Substrate.

---

## Deploying

Make sure your target environment variables are exported:

```bash
export PROJECT_ID="ax-substrate" # Your GCP project ID
export GEMINI_API_KEY="your-api-key"
export BUCKET_NAME="snapshot-substrate-test-$PROJECT_ID"
export KO_DOCKER_REPO="gcr.io/$PROJECT_ID/ate-images"
export KO_DEFAULTPLATFORMS="linux/amd64"
```

Render the template variables and apply the resolved manifest:

```bash
sed -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
    -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
    internal/manifests/ax-deployment2.yaml \
    | GOFLAGS="-tags=harness" ko apply -f -
```

Use the following command for initial deployment:

```bash
kubectl apply -f ax-deployment2.yaml
```

If you have a deployment and rolling out a new version, you can do:

```bash
kubectl delete pods -l app=ax-server -n ax 
kubectl delete workerpool ax-harness-workerpool -n ax
kubectl delete actortemplate ax-harness-template -n ax
```

Wait until all pods are up and running:

```bash
kubectl get pods -n ax
```

---

## Testing the Deployment

Proxy the `ax-server` ReplicaSet port to your local environment:

```bash
kubectl port-forward -n ax rs/ax-server 8494:8494
```

Execute a query against the local port-forwarded server using the `ax` CLI:

```bash
ax exec --server localhost:8494 --input "hello"
```

The server should respond with:
```text
Conversation: fb344a18-3720-4c4f-8a6e-2ce34db975b3

⏺ hello

Hello world
```
