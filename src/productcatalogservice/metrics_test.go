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

func TestMetricsUnaryInterceptor_Success(t *testing.T) {
	method := "/hipstershop.ProductCatalogService/ListProducts"
	startReq := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "OK").(prometheus.Counter))
	startHist := histogramSampleCount(t, grpcRequestDuration.WithLabelValues(method).(prometheus.Histogram))

	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }

	if _, err := metricsUnaryInterceptor(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "OK").(prometheus.Counter)); got != startReq+1 {
		t.Errorf("OK counter: got %v, want %v", got, startReq+1)
	}
	if got := histogramSampleCount(t, grpcRequestDuration.WithLabelValues(method).(prometheus.Histogram)); got != startHist+1 {
		t.Errorf("histogram samples: got %v, want %v", got, startHist+1)
	}
}

func TestMetricsUnaryInterceptor_Error(t *testing.T) {
	method := "/hipstershop.ProductCatalogService/ListProducts"
	startInternal := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Internal").(prometheus.Counter))

	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, status.Error(codes.Internal, "bad")
	}

	if _, err := metricsUnaryInterceptor(context.Background(), nil, info, handler); err == nil {
		t.Fatalf("expected error, got nil")
	}

	if got := counterValue(t, grpcRequestsTotal.WithLabelValues(method, "Internal").(prometheus.Counter)); got != startInternal+1 {
		t.Errorf("Internal counter: got %v, want %v", got, startInternal+1)
	}
}
