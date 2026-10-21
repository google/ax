package controller

import (
	"context"
	"log/slog"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
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

	// An empty atespace lists tasks across all atespaces. Keep paging until a
	// page comes back empty: ListTasks skips index entries whose record is
	// missing or unreadable, so a short page does not mean the end of the list.
	var tasks []*v1alpha1.Task
	const pageSize = 1000
	for offset := int64(0); ; offset += pageSize {
		page, err := s.ListTasks(ctx, "", pageSize, offset)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		tasks = append(tasks, page...)
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
