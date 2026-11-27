# Sessions

A session associates the tasks that belong to one conversation or one multi-turn piece of work. In AX a session is only an ID, `spec.sessionId`, that the control plane stores and passes to the agent. AX does not store the session's history or state; for now, bringing session storage is up to you.

## The session ID

Set `spec.sessionId` on every task that continues the same conversation:

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: fix-bug-turn-2
spec:
  sessionId: "session-42"   # optional; defaults to metadata.uid on creation
  goal:
    text: "Now add a unit test for the fix."
```

When a task leaves it empty, `CreateTask` sets it to the task's server-assigned `metadata.uid`, so every task has a session ID and a first turn can start a session without picking a name. A later turn reuses that ID by copying it from `ax describe task`. Like the rest of the spec, the session ID cannot change once the task is created.

AX does not check that tasks sharing a session ID are related, ordered, or in the same atespace. It is a correlation ID, and its meaning belongs to your agent.

## How the session ID is passed around

The session ID travels with the task wherever the task goes:

| Where | What you see |
|---|---|
| `ax describe task <name>` | A `Session ID:` line |
| The task pushed to the runner, `AX_TASK_YAML`, and `$AX_METADATA_URL/metadata/v1alpha1/ax/task` | `spec.sessionId` in the task YAML |
