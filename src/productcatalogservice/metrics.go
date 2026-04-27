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
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// Golden-signal collectors for the gRPC server. Method labels come from the
// gRPC FullMethod, bounded by the protobuf service definition rather than
// client input.
var (
	grpcRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_server_requests_total",
			Help: "Total number of gRPC requests handled, partitioned by method and status code.",
		},
		[]string{"method", "code"},
	)

	grpcRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_server_request_duration_seconds",
			Help:    "Latency of gRPC server requests in seconds, partitioned by method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	grpcInFlightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "grpc_server_in_flight_requests",
			Help: "Current number of gRPC requests being served (saturation signal).",
		},
	)
)

func registerMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		grpcRequestsTotal,
		grpcRequestDuration,
		grpcInFlightRequests,
	)
}

// startMetricsServer exposes /metrics on the given address. Runs on a separate
// port from the gRPC server so misconfigured ingress cannot publish /metrics.
func startMetricsServer(log logrus.FieldLogger, addr string, gatherer prometheus.Gatherer) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Infof("metrics server listening on %s/metrics", addr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Warnf("metrics server exited: %v", err)
		}
	}()
	return srv
}

// metricsUnaryInterceptor records request count, duration, and in-flight
// gauge for every gRPC request. Method label comes from info.FullMethod.
// Code label uses the canonical gRPC code string (OK, Unavailable, etc.).
func metricsUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	grpcInFlightRequests.Inc()
	defer grpcInFlightRequests.Dec()

	start := time.Now()
	resp, err := handler(ctx, req)
	elapsed := time.Since(start).Seconds()

	code := status.Code(err).String()
	grpcRequestsTotal.WithLabelValues(info.FullMethod, code).Inc()
	grpcRequestDuration.WithLabelValues(info.FullMethod).Observe(elapsed)

	return resp, err
}
