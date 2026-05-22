# Conversation snapshots and checkpoints

AX records conversation history as a sequenced event log. A **snapshot**
identifies a checkpoint in that log and is the first step toward a unified
resumption protocol (see `ForkConversationRequest.src_seq` TODO for
`snapshot_id` in `proto/ax.proto`).

## Snapshot format

```text
<conversation_id>:<seq>
```

Example:

```text
d85a4b4e-c53b-4c84-b879-f10d905bce40:12
```

- `seq` is the `ConversationEvent.seq` value (the same number printed as
  `seq=N` after `ax exec`).
- `seq` of `0` means **latest** (include all events).

## Package API

`internal/checkpoint` provides:

| Function | Use |
|----------|-----|
| `Parse` / `Snapshot.String` | Serialize checkpoints |
| `Truncate` | Fork up to a seq |
| `ValidateSeq` | Client catch-up (`last_seq`) |
| `EventsAfter` | Replay events after `last_seq` |
| `Latest` | Highest seq in a conversation |
| `ResolveForkSeq` | CLI `--src-snapshot` parsing |

## CLI usage

Fork with a raw sequence (existing):

```bash
ax fork \
  --src-conversation d85a4b4e-c53b-4c84-b879-f10d905bce40 \
  --dest-conversation e5e26e38-53a2-4f22-b1cb-ae867357df83 \
  --src-seq 12
```

Fork with a snapshot (preferred):

```bash
ax fork \
  --src-conversation d85a4b4e-c53b-4c84-b879-f10d905bce40 \
  --dest-conversation e5e26e38-53a2-4f22-b1cb-ae867357df83 \
  --src-snapshot d85a4b4e-c53b-4c84-b879-f10d905bce40:12
```

Client catch-up after disconnect (unchanged):

```bash
ax exec \
  --conversation d85a4b4e-c53b-4c84-b879-f10d905bce40 \
  --last-seq 12 \
  --resume
```

## Design notes

- Snapshots reference **conversation events** only today. Execution-scoped
  replay still uses `ExecutionEvent` entries keyed by `exec_id`.
- Invalid `src_seq` / snapshot seq values error instead of silently forking
  the full log.
- Future work: proto `snapshot_id`, `conversation_id` on execution log rows,
  merge `ConversationEvent` and `ExecutionEvent`.
