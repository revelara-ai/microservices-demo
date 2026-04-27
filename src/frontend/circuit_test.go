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
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	gobreaker "github.com/sony/gobreaker/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestLogger() logrus.FieldLogger {
	l := logrus.New()
	l.Out = io.Discard
	return l
}

func TestIsTransientGRPCError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"non-grpc error", errors.New("plain error"), true},
		{"unavailable", status.Error(codes.Unavailable, "down"), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "slow"), true},
		{"resource exhausted", status.Error(codes.ResourceExhausted, "quota"), true},
		{"aborted", status.Error(codes.Aborted, "concurrency"), true},
		{"canceled", status.Error(codes.Canceled, "cancel"), true},
		{"invalid argument", status.Error(codes.InvalidArgument, "bad input"), false},
		{"unauthenticated", status.Error(codes.Unauthenticated, "no token"), false},
		{"not found", status.Error(codes.NotFound, "missing"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientGRPCError(tc.err); got != tc.want {
				t.Errorf("isTransientGRPCError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCircuitBreaker_TripsOnTransientFailures verifies that repeated transient
// errors trip the circuit and that subsequent calls are rejected without
// invoking the wrapped function (fail-fast).
func TestCircuitBreaker_TripsOnTransientFailures(t *testing.T) {
	cb := newCircuitBreaker(newTestLogger(), "test", 1, 100*time.Millisecond, 200*time.Millisecond, 0.5, 3)
	transient := status.Error(codes.Unavailable, "down")

	// Drive enough transient failures to satisfy minRequests=3 and trip the
	// circuit (failureRatio=0.5 over 3 requests means 2+ failures).
	for i := 0; i < 3; i++ {
		_, _ = cbExecute(context.Background(), cb, func() (any, error) {
			return nil, transient
		})
	}

	// Circuit should now be open; the next call must fail fast without
	// invoking fn.
	called := false
	_, err := cbExecute(context.Background(), cb, func() (any, error) {
		called = true
		return "success", nil
	})
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Errorf("expected ErrOpenState after trip, got %v", err)
	}
	if called {
		t.Error("expected fn to be skipped while circuit is open")
	}
}

// TestCircuitBreaker_DoesNotTripOnClientErrors verifies that client errors
// (e.g., InvalidArgument) do NOT count as failures, so user-driven validation
// failures cannot accidentally trip the circuit.
func TestCircuitBreaker_DoesNotTripOnClientErrors(t *testing.T) {
	cb := newCircuitBreaker(newTestLogger(), "test", 1, 100*time.Millisecond, 200*time.Millisecond, 0.5, 3)
	clientErr := status.Error(codes.InvalidArgument, "bad request")

	for i := 0; i < 10; i++ {
		_, _ = cbExecute(context.Background(), cb, func() (any, error) {
			return nil, clientErr
		})
	}
	if state := cb.State(); state != gobreaker.StateClosed {
		t.Errorf("circuit should remain closed despite client errors; got state %v", state)
	}
}

// TestCircuitBreaker_HalfOpenRecovery verifies that after Timeout elapses, a
// successful probe transitions the breaker back to closed.
func TestCircuitBreaker_HalfOpenRecovery(t *testing.T) {
	cb := newCircuitBreaker(newTestLogger(), "test", 1, 50*time.Millisecond, 30*time.Millisecond, 0.5, 2)
	transient := status.Error(codes.Unavailable, "down")

	// Trip the circuit.
	for i := 0; i < 2; i++ {
		_, _ = cbExecute(context.Background(), cb, func() (any, error) {
			return nil, transient
		})
	}
	if cb.State() != gobreaker.StateOpen {
		t.Fatalf("expected open after failures; got %v", cb.State())
	}

	// Wait past Timeout so the breaker enters half-open on next call.
	time.Sleep(50 * time.Millisecond)

	// Successful probe in half-open should close the breaker.
	_, err := cbExecute(context.Background(), cb, func() (any, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("probe should succeed; got %v", err)
	}
	if cb.State() != gobreaker.StateClosed {
		t.Errorf("expected closed after successful probe; got %v", cb.State())
	}
}
