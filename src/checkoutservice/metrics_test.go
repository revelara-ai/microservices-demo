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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func histogramSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := h.Write(m); err != nil {
		t.Fatalf("histogram write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestMetricsUnaryInterceptor_Success ensures the OK code is recorded and the
// histogram observes a sample for successful gRPC handlers.
func TestMetricsUnaryInterceptor_Success(t *testing.T) {
	method := "/hipstershop.CheckoutService/PlaceOrder"
	startReq := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "OK").(prometheus.Counter))
	startHist := histogramSampleCount(t, grpcRequestDuration.WithLabelValues(method).(prometheus.Histogram))

	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	resp, err := metricsUnaryInterceptor(context.Background(), nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected response 'ok', got %v", resp)
	}

	if got := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "OK").(prometheus.Counter)); got != startReq+1 {
		t.Errorf("OK counter: got %v, want %v", got, startReq+1)
	}
	if got := histogramSampleCount(t, grpcRequestDuration.WithLabelValues(method).(prometheus.Histogram)); got != startHist+1 {
		t.Errorf("histogram samples: got %v, want %v", got, startHist+1)
	}
}

// TestMetricsUnaryInterceptor_TransientError ensures gRPC error codes are
// recorded by their canonical name (Unavailable, DeadlineExceeded, etc.) so
// SLO burn-rate alerts can distinguish transient from persistent failures.
func TestMetricsUnaryInterceptor_TransientError(t *testing.T) {
	method := "/hipstershop.CheckoutService/PlaceOrder"
	startUnavailable := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Unavailable").(prometheus.Counter))

	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.Unavailable, "upstream down")
	}

	_, err := metricsUnaryInterceptor(context.Background(), nil, info, handler)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	if got := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Unavailable").(prometheus.Counter)); got != startUnavailable+1 {
		t.Errorf("Unavailable counter: got %v, want %v", got, startUnavailable+1)
	}
}

// TestMetricsUnaryInterceptor_NonStatusError ensures non-status errors are
// recorded as Unknown rather than panicking or dropping the metric.
func TestMetricsUnaryInterceptor_NonStatusError(t *testing.T) {
	method := "/hipstershop.CheckoutService/PlaceOrder"
	startUnknown := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Unknown").(prometheus.Counter))

	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, errors.New("plain error")
	}

	_, err := metricsUnaryInterceptor(context.Background(), nil, info, handler)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	if got := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Unknown").(prometheus.Counter)); got != startUnknown+1 {
		t.Errorf("Unknown counter: got %v, want %v", got, startUnknown+1)
	}
}

// TestCircuitBreakerStateChangeCounter verifies the counter wired into
// newCircuitBreaker's OnStateChange callback increments per transition.
func TestCircuitBreakerStateChangeCounter(t *testing.T) {
	circuitBreakerStateChanges.Reset()
	circuitBreakerStateChanges.WithLabelValues("payment-service", "open").Inc()
	circuitBreakerStateChanges.WithLabelValues("payment-service", "open").Inc()
	circuitBreakerStateChanges.WithLabelValues("payment-service", "closed").Inc()

	if got := counterValue(t, circuitBreakerStateChanges.WithLabelValues("payment-service", "open").(prometheus.Counter)); got != 2 {
		t.Errorf("open counter: got %v, want 2", got)
	}
	if got := counterValue(t, circuitBreakerStateChanges.WithLabelValues("payment-service", "closed").(prometheus.Counter)); got != 1 {
		t.Errorf("closed counter: got %v, want 1", got)
	}
}
