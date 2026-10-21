package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

type fakeLister struct {
	actors []*ateapipb.Actor
	err    error
}

func (f fakeLister) ListAllActors(ctx context.Context) ([]*ateapipb.Actor, error) {
	return f.actors, f.err
}

// fakeStore embeds the interface so only ListTasks needs to be implemented.
type fakeStore struct {
	store.Store
	tasks []*v1alpha1.Task
	err   error
	calls *int // counts ListTasks calls, when non-nil
}

func (f fakeStore) ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	if f.calls != nil {
		*f.calls++
	}
	if f.err != nil {
		return nil, f.err
	}
	if offset >= int64(len(f.tasks)) {
		return nil, nil
	}
	end := min(offset+limit, int64(len(f.tasks)))
	return f.tasks[offset:end], nil
}

func TestLogOrphanActors(t *testing.T) {
	ctx := context.Background()

	t.Run("runs without error when there are orphans", func(t *testing.T) {
		lister := fakeLister{actors: []*ateapipb.Actor{actor("default", "gone")}}
		s := fakeStore{tasks: []*v1alpha1.Task{task("default", "alive")}}
		if err := LogOrphanActors(ctx, lister, s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("returns the actor listing error", func(t *testing.T) {
		want := errors.New("substrate down")
		lister := fakeLister{err: want}
		if err := LogOrphanActors(ctx, lister, fakeStore{}); !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	})

	t.Run("returns the task listing error", func(t *testing.T) {
		want := errors.New("redis down")
		lister := fakeLister{actors: []*ateapipb.Actor{actor("default", "a")}}
		if err := LogOrphanActors(ctx, lister, fakeStore{err: want}); !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	})

	t.Run("reads every task in one call", func(t *testing.T) {
		// 1,001 tasks: a paged read would need two calls; this one needs one.
		var tasks []*v1alpha1.Task
		for i := 0; i < 1001; i++ {
			tasks = append(tasks, task("default", "t"))
		}
		calls := 0
		s := fakeStore{tasks: tasks, calls: &calls}
		if err := LogOrphanActors(ctx, fakeLister{}, s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// All tasks come back in a single read, so there is no paging to skip.
		if calls != 1 {
			t.Fatalf("ListTasks called %d times, want 1", calls)
		}
	})
}
