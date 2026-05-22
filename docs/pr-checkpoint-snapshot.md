## Summary

Introduce a conversation **Snapshot** checkpoint type and centralize fork /
client catch-up logic. First step toward unified resumption protocol; no proto
or SQLite schema changes.

- Adds `internal/checkpoint` with parse/format, truncate, validate, and replay helpers
- Refactors `controller.Fork` and `tryResuming` (`last_seq`) to use the package
- Adds `ax fork --src-snapshot <conversation_id>:<seq>` (alias for `--src-seq`)
- Documents snapshot format in `docs/checkpoint-resumption.md`

## Motivation

Checkpoint handling was duplicated in the controller and tied to raw integers.
Proto already notes replacing `src_seq` with `snapshot_id`; this PR introduces
the Go-side snapshot type without a wire break.

## Test plan

- [x] `go test ./internal/checkpoint/...`
- [x] `go test ./internal/controller/...` (fork + resumption tests)
- [x] `make test`
- [x] `make build`
- [ ] Manual: `ax fork --src-snapshot conv:1 --src-conversation conv ...`

## Breaking changes

None. `--src-seq` and `last_seq` behavior unchanged.

## Follow-ups

- Add `snapshot_id` to `ForkConversationRequest` proto
- Add `conversation_id` column to `execution_log`
- Merge conversation and execution event types

## Checklist

- [ ] Signed [Google CLA](https://cla.developers.google.com/)
- [ ] Tracking issue on google/ax (optional)
