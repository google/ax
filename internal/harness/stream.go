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

package harness

import (
	"context"
	"fmt"
	"io"

	"github.com/google/ax/proto"
)

// drainStream reads from the harness gRPC stream until io.EOF, dispatching messages
// to the handler, and returns the final execution status.
func drainStream(ctx context.Context, stream proto.HarnessService_ConnectClient, execID string, handler Handler) error {
	var endState proto.State
	var endErr error
	hasEnd := false

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("gRPC harness streaming failure: %w", err)
		}

		switch payload := resp.Type.(type) {
		case *proto.HarnessResponse_Outputs:
			for _, outMsg := range payload.Outputs.Messages {
				if err := handler.OnMessage(ctx, execID, outMsg); err != nil {
					return fmt.Errorf("failed to dispatch streamed output: %w", err)
				}
			}
		case *proto.HarnessResponse_End:
			hasEnd = true
			endState = payload.End.GetState()
			if endState == proto.State_STATE_FAILED {
				if errDetail := payload.End.GetError(); errDetail != nil {
					endErr = fmt.Errorf("harness failed: [%d] %s", errDetail.GetCode(), errDetail.GetDescription())
				} else {
					endErr = fmt.Errorf("harness failed with no error details")
				}
			}
		}
	}

	if !hasEnd {
		return fmt.Errorf("harness stream ended without HarnessEnd frame")
	}
	if endState == proto.State_STATE_FAILED {
		return endErr
	}
	return handler.OnComplete(ctx, execID)
}
