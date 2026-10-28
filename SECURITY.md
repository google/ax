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
