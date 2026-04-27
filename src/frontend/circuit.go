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
	"time"

	"github.com/sirupsen/logrus"
	gobreaker "github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newCircuitBreaker returns a gobreaker configured for an upstream gRPC service.
// Only transient gRPC errors (see isTransientGRPCError) count toward tripping;
// minRequests must be observed in the rolling window before failureRatio is
// evaluated, preventing trips on a single error during low traffic.
func newCircuitBreaker(log logrus.FieldLogger, name string, maxRequests uint32, interval, timeout time.Duration, failureRatio float64, minRequests uint32) *gobreaker.CircuitBreaker[any] {
	return gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
		Name:        name,
		MaxRequests: maxRequests,
		Interval:    interval,
		Timeout:     timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.Requests >= minRequests &&
				float64(counts.TotalFailures)/float64(counts.Requests) >= failureRatio
		},
		IsSuccessful: func(err error) bool {
			return !isTransientGRPCError(err)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.WithFields(logrus.Fields{
				"circuit": name,
				"from":    from.String(),
				"to":      to.String(),
			}).Warn("circuit breaker state changed")
		},
	})
}

// isTransientGRPCError returns true for gRPC errors that indicate a transient
// upstream issue and should count toward the circuit breaker failure threshold.
// Client errors (InvalidArgument, Unauthenticated, etc.) return false so they
// don't trip the circuit on user-driven validation failures.
func isTransientGRPCError(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return true
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted, codes.Canceled:
		return true
	default:
		return false
	}
}

// cbExecute wraps a circuit breaker call with OTel span attributes for
// observability. When the breaker rejects the call (open or too many requests
// in half-open), it tags the span with circuit.rejected=true so traces can be
// filtered.
func cbExecute(ctx context.Context, cb *gobreaker.CircuitBreaker[any], fn func() (any, error)) (any, error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("circuit.name", cb.Name()),
		attribute.String("circuit.state", cb.State().String()),
	)
	result, err := cb.Execute(fn)
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		span.SetAttributes(attribute.Bool("circuit.rejected", true))
	}
	return result, err
}
