# Networking

Tasks do not get a Kubernetes Service or Ingress of their own. Every request to a task goes through Agent Substrate's **atenet router**, the `atenet-router` Service in the `ate-system` namespace. The router reads a single header, `ate-target-actor`, resolves the actor to the worker it is running on, resumes it first if it was suspended, and proxies the request there. `Host` and `:authority` are left alone for your application; the header alone selects the target.

The header value is `<atespace>/<task>`. AX always names a task's actor after the task, so `default/task123` reaches the task `task123` in the `default` atespace.

## From inside the cluster

Use the Service DNS name and add the header. This is exactly how AX polls a task's readiness.

```bash
curl -H "ate-target-actor: default/task123" \
  http://atenet-router.ate-system.svc.cluster.local/metadata/v1alpha1/ax/task
```

## From your machine

Port-forward the router, then talk to it the same way.

```bash
kubectl -n ate-system port-forward svc/atenet-router 8001:80
curl -H "ate-target-actor: default/task123" http://localhost:8001/readyz
```

## gRPC request routing

Send the header as outgoing metadata under the lowercase key. This is what `ax ssh` does to reach the guest services.

```go
ctx = metadata.AppendToOutgoingContext(ctx, "ate-target-actor", "default/task123")
resp, err := client.SomeMethod(ctx, req)
```
