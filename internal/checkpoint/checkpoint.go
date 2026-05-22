// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package checkpoint provides conversation snapshot identifiers and helpers
// for fork, resume catch-up, and future snapshot_id wire formats.
package checkpoint

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/ax/proto"
)

// Snapshot identifies a point in a conversation event log. Seq 0 means the
// latest event (include the full log). The string form is "<conversation_id>:<seq>"
// and is intended to become the snapshot_id representation on the wire.
type Snapshot struct {
	ConversationID string
	Seq            int32
}

// At returns a snapshot for conversationID at seq. Seq 0 selects the latest event.
func At(conversationID string, seq int32) Snapshot {
	return Snapshot{ConversationID: conversationID, Seq: seq}
}

// String formats the snapshot as "<conversation_id>:<seq>".
func (s Snapshot) String() string {
	return fmt.Sprintf("%s:%d", s.ConversationID, s.Seq)
}

// Parse parses "<conversation_id>:<seq>". Seq may be 0 for latest.
func Parse(raw string) (Snapshot, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Snapshot{}, fmt.Errorf("checkpoint: empty snapshot")
	}
	i := strings.LastIndex(raw, ":")
	if i <= 0 || i == len(raw)-1 {
		return Snapshot{}, fmt.Errorf("checkpoint: invalid snapshot %q (want <conversation_id>:<seq>)", raw)
	}
	conversationID := raw[:i]
	seqStr := raw[i+1:]
	seq64, err := strconv.ParseInt(seqStr, 10, 32)
	if err != nil {
		return Snapshot{}, fmt.Errorf("checkpoint: invalid seq in snapshot %q: %w", raw, err)
	}
	if seq64 < 0 {
		return Snapshot{}, fmt.Errorf("checkpoint: seq must be non-negative in snapshot %q", raw)
	}
	return Snapshot{ConversationID: conversationID, Seq: int32(seq64)}, nil
}

// Latest returns a snapshot at the highest seq in events, or Seq 0 when empty.
func Latest(conversationID string, events []*proto.ConversationEvent) Snapshot {
	var maxSeq int32
	for _, ev := range events {
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	return Snapshot{ConversationID: conversationID, Seq: maxSeq}
}

// ValidateSeq reports whether seq appears in events. Seq 0 is always valid.
func ValidateSeq(events []*proto.ConversationEvent, seq int32) error {
	if seq == 0 {
		return nil
	}
	for _, ev := range events {
		if ev.Seq == seq {
			return nil
		}
	}
	return fmt.Errorf("seq %d not found", seq)
}

// Truncate returns events up to and including snap.Seq. When snap.Seq is 0,
// all events are returned. When snap.Seq is set, it must exist in events.
func Truncate(events []*proto.ConversationEvent, snap Snapshot) ([]*proto.ConversationEvent, error) {
	if snap.Seq == 0 {
		return events, nil
	}
	for i, ev := range events {
		if ev.Seq == snap.Seq {
			return events[:i+1], nil
		}
		if ev.Seq > snap.Seq {
			break
		}
	}
	if snap.ConversationID != "" {
		return nil, fmt.Errorf("seq %d not found in conversation %s", snap.Seq, snap.ConversationID)
	}
	return nil, fmt.Errorf("seq %d not found", snap.Seq)
}

// EventsAfter returns conversation events with seq strictly greater than afterSeq,
// preserving order.
func EventsAfter(events []*proto.ConversationEvent, afterSeq int32) []*proto.ConversationEvent {
	if afterSeq == 0 {
		return nil
	}
	out := make([]*proto.ConversationEvent, 0)
	for _, ev := range events {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out
}

// ResolveForkSeq returns the sequence number to fork from. When snapshot is
// non-empty it must parse as "<conversation_id>:<seq>" and match conversationID.
func ResolveForkSeq(conversationID, snapshot string, fallbackSeq int32) (int32, error) {
	if snapshot == "" {
		return fallbackSeq, nil
	}
	snap, err := Parse(snapshot)
	if err != nil {
		return 0, err
	}
	if snap.ConversationID != conversationID {
		return 0, fmt.Errorf("checkpoint: snapshot conversation %q does not match %q", snap.ConversationID, conversationID)
	}
	return snap.Seq, nil
}
