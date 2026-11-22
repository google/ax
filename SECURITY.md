# Security Policy

Please use https://g.co/vulnz to report security vulnerabilities.

We use https://g.co/vulnz for our intake and triage. For valid issues we will do coordination and disclosure here on GitHub (including using a GitHub Security Advisory when necessary).

The Google Security Team will process your report within a day, and respond within a week (although it will depend on the severity of your report).

## Current trust boundary

AX is in heavy development. Until authentication and isolation land, treat a deployment as follows:

- The `ax-server` API has no authentication or authorization. Anyone who can reach it can read, create, suspend, resume and delete any Task, Workspace or Model. Only expose it to trusted clients.
- Atespaces organize resources. They are not a tenant isolation boundary.
- Setting `spec.debug: true` turns on the guest services, which allow arbitrary command execution and file access inside the sandbox. Whoever can create a Task can set it.
- Anything inside a task's sandbox, including the agent and the task runner, should be treated as untrusted. Don't rely on values reported from inside the sandbox for security decisions.

## Opt-in task log streaming

Task log streaming must stay off by default. Before enabling it, restrict
API access to trusted clients: the API still has no authentication or
atespace authorization ([#376](https://github.com/google/ax/issues/376)). Any
reachable client can request another task's logs. Filtering by actor UID
selects records; it does not authorize their reader.

Task output is untrusted and is forwarded without secret redaction. A real
`GEMINI_API_KEY` or other credential in a task can appear in stdout/stderr and
therefore in worker logs and this stream. Use egress credential injection
and placeholders instead of real credentials in tasks
([#475](https://github.com/google/ax/issues/475)). Enabling streaming does not
resolve either risk.

Apply the optional RBAC manifest with
`kubectl apply -n <worker-pool-namespace> -f deploy/task-logs-rbac.yaml` in
each worker-pool namespace. Set the RoleBinding subject namespace to the
namespace where ax-server runs (`ax-system` by default). The manifest grants
`get` on `pods/log` in the selected namespace. Because Kubernetes cannot
restrict this permission by actor UID, ax-server can read all container logs
there. Bind it only in namespaces whose worker logs the deployment is trusted
to read.
