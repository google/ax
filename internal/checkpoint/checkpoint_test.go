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

package checkpoint

import (
	"strings"
	"testing"

	"github.com/google/ax/proto"
)

func events(seqs ...int32) []*proto.ConversationEvent {
	out := make([]*proto.ConversationEvent, len(seqs))
	for i, seq := range seqs {
		out[i] = &proto.ConversationEvent{ConversationId: "conv-1", Seq: seq}
	}
	return out
}

func TestParse(t *testing.T) {
	snap, err := Parse("d85a4b4e-c53b-4c84-b879-f10d905bce40:12")
	if err != nil {
		t.Fatal(err)
	}
	if snap.ConversationID != "d85a4b4e-c53b-4c84-b879-f10d905bce40" || snap.Seq != 12 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if got := snap.String(); got != "d85a4b4e-c53b-4c84-b879-f10d905bce40:12" {
		t.Fatalf("String() = %q", got)
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := []string{"", "noseparator", "conv:", ":12", "conv:abc", "conv:-1"}
	for _, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) expected error", raw)
		}
	}
}

func TestLatest(t *testing.T) {
	snap := Latest("conv-1", events(1, 3, 2))
	if snap.Seq != 3 || snap.ConversationID != "conv-1" {
		t.Fatalf("unexpected latest: %+v", snap)
	}
}

func TestTruncate(t *testing.T) {
	evs := events(1, 2, 3)

	all, err := Truncate(evs, At("conv-1", 0))
	if err != nil || len(all) != 3 {
		t.Fatalf("truncate latest: len=%d err=%v", len(all), err)
	}

	partial, err := Truncate(evs, At("conv-1", 2))
	if err != nil || len(partial) != 2 || partial[1].Seq != 2 {
		t.Fatalf("truncate at 2: %+v err=%v", partial, err)
	}

	if _, err := Truncate(evs, At("conv-1", 99)); err == nil {
		t.Fatal("expected error for missing seq")
	}
}

func TestValidateSeq(t *testing.T) {
	evs := events(1, 2)
	if err := ValidateSeq(evs, 0); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSeq(evs, 2); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSeq(evs, 9); err == nil {
		t.Fatal("expected error")
	}
}

func TestEventsAfter(t *testing.T) {
	evs := events(1, 2, 3)
	after := EventsAfter(evs, 1)
	if len(after) != 2 || after[0].Seq != 2 || after[1].Seq != 3 {
		t.Fatalf("unexpected after events: %+v", after)
	}
	if len(EventsAfter(evs, 0)) != 0 {
		t.Fatal("expected no events after seq 0")
	}
}

func TestAtRoundTrip(t *testing.T) {
	s := At("abc", 5)
	parsed, err := Parse(s.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != s {
		t.Fatalf("round trip mismatch: %+v vs %+v", parsed, s)
	}
	if !strings.Contains(s.String(), ":5") {
		t.Fatalf("unexpected string: %s", s.String())
	}
}

func TestResolveForkSeq(t *testing.T) {
	seq, err := ResolveForkSeq("conv-1", "conv-1:2", 0)
	if err != nil || seq != 2 {
		t.Fatalf("ResolveForkSeq snapshot: seq=%d err=%v", seq, err)
	}

	seq, err = ResolveForkSeq("conv-1", "", 3)
	if err != nil || seq != 3 {
		t.Fatalf("ResolveForkSeq fallback: seq=%d err=%v", seq, err)
	}

	if _, err := ResolveForkSeq("conv-1", "other:2", 0); err == nil {
		t.Fatal("expected conversation mismatch error")
	}
}
