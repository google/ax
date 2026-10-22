package substrate

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
)

// fakeControl overrides only ListActors; the embedded interface is nil, so any
// other call would panic and show up in the test.
type fakeControl struct {
	ateapipb.ControlClient
	pages       []*ateapipb.ListActorsResponse
	tokens      []string
	err         error // returned on every call when set
	errOnSecond error // returned only on the second call when set
}

func (f *fakeControl) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	f.tokens = append(f.tokens, req.GetOptions().GetPageToken())
	if f.err != nil {
		return nil, f.err
	}
	if len(f.tokens) == 2 && f.errOnSecond != nil {
		return nil, f.errOnSecond
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func TestListAllActorsFollowsPages(t *testing.T) {
	fc := &fakeControl{pages: []*ateapipb.ListActorsResponse{
		{Actors: []*ateapipb.Actor{{Metadata: &ateapipb.ResourceMetadata{Name: "a"}}}, NextPageToken: "next"},
		{Actors: []*ateapipb.Actor{{Metadata: &ateapipb.ResourceMetadata{Name: "b"}}}},
	}}
	c := &Client{control: fc}

	got, err := c.ListAllActors(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].GetMetadata().GetName() != "a" || got[1].GetMetadata().GetName() != "b" {
		t.Fatalf("got %d actors, want a then b", len(got))
	}
	if len(fc.tokens) != 2 || fc.tokens[0] != "" || fc.tokens[1] != "next" {
		t.Fatalf("page tokens = %q, want [\"\" \"next\"]", fc.tokens)
	}
}

func TestListAllActorsReturnsError(t *testing.T) {
	want := errors.New("boom")
	c := &Client{control: &fakeControl{err: want}}
	if _, err := c.ListAllActors(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestListAllActorsReturnsErrorOnSecondPage(t *testing.T) {
	fc := &fakeControl{
		pages: []*ateapipb.ListActorsResponse{
			{Actors: []*ateapipb.Actor{{Metadata: &ateapipb.ResourceMetadata{Name: "a"}}}, NextPageToken: "next"},
		},
		errOnSecond: errors.New("second page failed"),
	}
	c := &Client{control: fc}
	if _, err := c.ListAllActors(context.Background()); err == nil {
		t.Fatal("expected an error from the second page, got nil")
	}
}
