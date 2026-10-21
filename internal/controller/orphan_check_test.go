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

	t.Run("reads every page of tasks", func(t *testing.T) {
		// 1,001 tasks needs two pages of 1,000: a full page, then a short one.
		var tasks []*v1alpha1.Task
		for i := 0; i < 1001; i++ {
			tasks = append(tasks, task("default", "t"))
		}
		calls := 0
		s := fakeStore{tasks: tasks, calls: &calls}
		if err := LogOrphanActors(ctx, fakeLister{}, s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Two full-or-short pages with tasks, then one empty page that ends the loop.
		if calls != 3 {
			t.Fatalf("ListTasks called %d times, want 3", calls)
		}
	})

	t.Run("a short page in the middle does not end paging early", func(t *testing.T) {
		// Page one returns only 2 tasks (dangling index entries were skipped),
		// but page two still has data; the check must keep reading.
		s := &gappyStore{pages: [][]*v1alpha1.Task{
			{task("default", "a"), task("default", "b")},
			{task("default", "c")},
		}}
		if err := LogOrphanActors(ctx, fakeLister{}, s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.calls != 3 {
			t.Fatalf("ListTasks called %d times, want 3", s.calls)
		}
	})
}

// gappyStore returns the given pages in order, then empty pages.
type gappyStore struct {
	store.Store
	pages [][]*v1alpha1.Task
	calls int
}

func (g *gappyStore) ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	g.calls++
	if g.calls-1 < len(g.pages) {
		return g.pages[g.calls-1], nil
	}
	return nil, nil
}
