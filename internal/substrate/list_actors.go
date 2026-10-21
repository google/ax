package substrate

import (
	"context"
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
)

// ListAllActors returns every actor across all atespaces, following pagination.
func (c *Client) ListAllActors(ctx context.Context) ([]*ateapipb.Actor, error) {
	var all []*ateapipb.Actor
	pageToken := ""
	for {
		var resp *ateapipb.ListActorsResponse
		err := retryOnCodes(ctx, func() error {
			var err error
			resp, err = c.control.ListActors(ctx, &ateapipb.ListActorsRequest{
				PageSize:  1000,
				PageToken: pageToken,
			})
			return err
		}, codes.Unavailable)
		if err != nil {
			return nil, fmt.Errorf("listing actors: %w", err)
		}
		all = append(all, resp.GetActors()...)
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return all, nil
		}
	}
}
