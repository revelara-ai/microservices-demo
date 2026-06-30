// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TestRunReturnsServingServer verifies run() returns live server handles and
// that the gRPC health check reports SERVING before any shutdown.
func TestRunReturnsServingServer(t *testing.T) {
	srv, healthcheck := run("0")
	if srv == nil || healthcheck == nil {
		t.Fatalf("run returned nil handles: srv=%v healthcheck=%v", srv, healthcheck)
	}
	defer gracefulShutdown(srv, healthcheck, nil)

	resp, err := healthcheck.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING before shutdown, got %v", resp.GetStatus())
	}
}

// TestGracefulShutdownDrainsAndFailsReadiness verifies that gracefulShutdown
// flips the health status to NOT_SERVING (so the readiness probe fails fast and
// the pod is pulled from rotation) and drains the gRPC server so it no longer
// accepts new RPCs. This is the SIGTERM-handling control path that R-002 adds.
func TestGracefulShutdownDrainsAndFailsReadiness(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	srv := grpc.NewServer()
	healthcheck := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthcheck)
	go func() { _ = srv.Serve(lis) }()

	ctx := context.Background()

	// The default ("") service is SERVING immediately after registration.
	resp, err := healthcheck.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("pre-shutdown health check failed: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING before shutdown, got %v", resp.GetStatus())
	}

	// gracefulShutdown should return promptly when there are no in-flight RPCs.
	done := make(chan struct{})
	go func() {
		gracefulShutdown(srv, healthcheck, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gracefulShutdown did not complete within 10s")
	}

	// Readiness must now fail fast.
	resp, err = healthcheck.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("post-shutdown health check failed: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING after shutdown, got %v", resp.GetStatus())
	}

	// The drained server must reject new RPCs.
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer conn.Close()

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(callCtx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("expected RPC to fail against a drained server, got nil error")
	}
}
