package controller

import (
	"context"
	"log/slog"
	"math"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/store"
)

// ActorLister lists every Substrate actor. *substrate.Client satisfies it.
type ActorLister interface {
	ListAllActors(ctx context.Context) ([]*ateapipb.Actor, error)
}

// LogOrphanActors logs every Substrate actor that has no matching Task record.
// It only logs; it never deletes anything.
func LogOrphanActors(ctx context.Context, client ActorLister, s store.Store) error {
	// Actors are listed before tasks on purpose. A task saved after the actor
	// list cannot cause a false orphan, because its actor is not in that list.
	// A task deleted after the actor list can still cause one false warning.
	// That is acceptable here because this check only logs.
	actors, err := client.ListAllActors(ctx)
	if err != nil {
		return err
	}

	// Read every task in one call. Paging by offset is unsafe here: a task
	// deleted between pages shifts the list, so the scan can skip a task and
	// log a live actor as an orphan. An empty atespace lists all atespaces.
	const allTasks = math.MaxInt32
	tasks, err := s.ListTasks(ctx, "", allTasks, 0)
	if err != nil {
		return err
	}

	orphans := FindOrphanActors(actors, tasks)
	for _, a := range orphans {
		slog.Warn("orphan actor has no task record",
			"atespace", a.GetMetadata().GetAtespace(),
			"actor", a.GetMetadata().GetName())
	}
	slog.Info("orphan check finished", "actors", len(actors), "tasks", len(tasks), "orphans", len(orphans))
	return nil
}
