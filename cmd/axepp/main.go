
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

// Command axepp is an Envoy External Processing (ext_authz) plugin for the ax server.
// It intercepts Exec, DeleteConversation, and ForkConversation gRPC requests to
// extract conversation IDs and safely initiate session/actor resumption.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/google/ax/proto"
	gapistatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	protov2 "google.golang.org/protobuf/proto"
)

var (
	port                   = flag.Int("port", 50051, "The port for the gRPC authorization server to listen on")
	axPort                 = flag.String("ax-port", "8494", "The port where the AX server is listening on")
	grpcServerCredBundle   = flag.String("grpc-server-cred-bundle", "", "File with the server TLS credential bundle.")
	actorTemplateNamespace = flag.String("actor-template-namespace", "ax", "The Actor Template namespace to use for resuming sessions.")
	actorTemplateName      = flag.String("actor-template-name", "ax-template", "The Actor Template name to use for resuming sessions.")
)

type authServer struct {
	sc ateapipb.ControlClient
}

func runSession(ctx context.Context, sc ateapipb.ControlClient, sessionID string) (*authv3.CheckResponse, error) {
	slog.InfoContext(ctx, "About to call CreateActor", slog.Any("actor-id", sessionID))
	if _, err := sc.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId:                sessionID,
		ActorTemplateNamespace: *actorTemplateNamespace,
		ActorTemplateName:      *actorTemplateName,
	}); err != nil {
		if status.Code(err) != codes.AlreadyExists {
			slog.ErrorContext(ctx, "CreateActor error", slog.Any("error", err))
			return &authv3.CheckResponse{
				Status: &gapistatus.Status{Code: int32(codes.Unavailable), Message: err.Error()},
			}, nil
		}
	}

	slog.InfoContext(ctx, "About to call ResumeActor", slog.Any("actor-id", sessionID))
	resp, err := sc.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
		ActorId: sessionID,
	})
	if err != nil {
		slog.InfoContext(ctx, "ResumeActor error", slog.Any("error", err))
		return &authv3.CheckResponse{
			Status: &gapistatus.Status{Code: int32(codes.Unavailable), Message: err.Error()},
		}, nil
	}

	destinationIP := resp.GetActor().GetAteomPodIp()
	destrinationAddr := net.JoinHostPort(destinationIP, *axPort)
	slog.InfoContext(ctx, "Redirecting request to backend", slog.String("address", destrinationAddr))

	var headers []*corepb.HeaderValueOption

	headers = append(headers, &corepb.HeaderValueOption{
		Header: &corepb.HeaderValue{
			Key:   "x-backend-ip",
			Value: destrinationAddr,
		},
	})

	return &authv3.CheckResponse{
		Status: &gapistatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{
				Headers: headers,
			},
		},
	}, nil
}

func route(ctx context.Context, sc ateapipb.ControlClient, body []byte, req protov2.Message, pathName string, extractor func() string) (*authv3.CheckResponse, error) {
	if len(body) <= 5 {
		return nil, fmt.Errorf("body too short for path %s", pathName)
	}

	payload := body[5:]
	if err := protov2.Unmarshal(payload, req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal request for path %s: %w", pathName, err)
	}
	conversationID := extractor()
	slog.InfoContext(ctx, "Extracted conversation ID", slog.String("path", pathName), slog.String("conversation_id", conversationID))
	if conversationID == "" {
		return nil, fmt.Errorf("empty conversation ID extracted for path %s", pathName)
	}
	return runSession(ctx, sc, conversationID)
}

func (s *authServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpReq := req.GetAttributes().GetRequest().GetHttp()
	path := httpReq.GetPath()

	slog.InfoContext(ctx, "axepp Check received request", slog.String("method", httpReq.GetMethod()), slog.String("path", path))

	defaultResp := &authv3.CheckResponse{
		Status: &gapistatus.Status{Code: int32(codes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{},
		},
	}

	body := httpReq.GetRawBody()

	switch path {
	case "/ax.ControllerService/Exec":
		var execReq proto.ExecRequest
		resp, err := route(ctx, s.sc, body, &execReq, path, func() string { return execReq.GetConversationId() })
		if err != nil {
			slog.ErrorContext(ctx, "EPP routing failed", slog.String("path", path), slog.Any("error", err))
		}
		return resp, err
	case "/ax.ConversationService/DeleteConversation":
		var delReq proto.DeleteConversationRequest
		resp, err := route(ctx, s.sc, body, &delReq, path, func() string { return delReq.GetConversationId() })
		if err != nil {
			slog.ErrorContext(ctx, "EPP routing failed", slog.String("path", path), slog.Any("error", err))
		}
		return resp, err
	case "/ax.ConversationService/ForkConversation":
		var forkReq proto.ForkConversationRequest
		resp, err := route(ctx, s.sc, body, &forkReq, path, func() string { return forkReq.GetDestConversationId() })
		if err != nil {
			slog.ErrorContext(ctx, "EPP routing failed", slog.String("path", path), slog.Any("error", err))
		}
		return resp, err
	}
	return defaultResp, nil
}

func main() {
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	scAddress := "api.ate-system.svc:443"
	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	if *grpcServerCredBundle != "" {
		bundleBytes, err := os.ReadFile(*grpcServerCredBundle)
		if err != nil {
			slog.Error("Failed to read bundle", slog.Any("error", err))
			os.Exit(1)
		}
		certPool := x509.NewCertPool()
		if ok := certPool.AppendCertsFromPEM(bundleBytes); ok {
			clientTLSConfig.RootCAs = certPool
		}
	}

	clientCreds := credentials.NewTLS(clientTLSConfig)
	conn, err := grpc.NewClient(scAddress, grpc.WithTransportCredentials(clientCreds))
	if err != nil {
		slog.Error("did not connect to substrate control", slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Close()

	sc := ateapipb.NewControlClient(conn)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		slog.Error("Failed to listen", slog.Int("port", *port), slog.Any("error", err))
		os.Exit(1)
	}

	s := grpc.NewServer()
	as := &authServer{sc: sc}
	authv3.RegisterAuthorizationServer(s, as)

	slog.Info("ax Envoy Authorization Service (ext_authz) listening", slog.Any("address", lis.Addr()))
	if err := s.Serve(lis); err != nil {
		slog.Error("Failed to serve gRPC server", slog.Any("error", err))
		os.Exit(1)
	}
}
