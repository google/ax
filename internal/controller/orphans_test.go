package controller

import (
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func actor(atespace, name string) *ateapipb.Actor {
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name}}
}

func task(atespace, name string) *v1alpha1.Task {
	return &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Atespace: atespace, Name: name}}
}

func TestFindOrphanActors(t *testing.T) {
	tests := []struct {
		name   string
		actors []*ateapipb.Actor
		tasks  []*v1alpha1.Task
		want   []string // "atespace/name" of expected orphans
	}{
		{
			name:   "no actors",
			actors: nil,
			tasks:  []*v1alpha1.Task{task("default", "a")},
			want:   nil,
		},
		{
			name:   "matched pair is not an orphan",
			actors: []*ateapipb.Actor{actor("default", "a")},
			tasks:  []*v1alpha1.Task{task("default", "a")},
			want:   nil,
		},
		{
			name:   "actor with no task is an orphan",
			actors: []*ateapipb.Actor{actor("default", "a"), actor("default", "b")},
			tasks:  []*v1alpha1.Task{task("default", "a")},
			want:   []string{"default/b"},
		},
		{
			name:   "same name in another atespace is an orphan",
			actors: []*ateapipb.Actor{actor("team-x", "a")},
			tasks:  []*v1alpha1.Task{task("default", "a")},
			want:   []string{"team-x/a"},
		},
		{
			name:   "no tasks means every actor is an orphan",
			actors: []*ateapipb.Actor{actor("default", "a")},
			tasks:  nil,
			want:   []string{"default/a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindOrphanActors(tc.actors, tc.tasks)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d orphans, want %d", len(got), len(tc.want))
			}
			for i, a := range got {
				key := a.GetMetadata().GetAtespace() + "/" + a.GetMetadata().GetName()
				if key != tc.want[i] {
					t.Errorf("orphan %d = %q, want %q", i, key, tc.want[i])
				}
			}
		})
	}
}
