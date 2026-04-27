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
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// Golden-signal collectors. Labels stay bounded by route count rather than
// request count: handler labels come from mux route templates (e.g.,
// /product/{id}), never from raw URL paths. Collectors are package-level
// so the gobreaker OnStateChange callback in circuit.go can also write to
// circuitBreakerStateChanges (R-005).
var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests handled by the frontend, partitioned by route template and response status code.",
		},
		[]string{"handler", "code"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Latency of frontend HTTP requests in seconds, partitioned by route template.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"handler"},
	)

	httpInFlightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_in_flight_requests",
			Help: "Current number of HTTP requests being served by the frontend (saturation signal).",
		},
	)

	circuitBreakerStateChanges = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "circuit_breaker_state_changes_total",
			Help: "Number of frontend circuit breaker state transitions, partitioned by upstream service and target state.",
		},
		[]string{"circuit", "to"},
	)
)

func registerMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpInFlightRequests,
		circuitBreakerStateChanges,
	)
}

// startMetricsServer exposes /metrics on the given address using the supplied
// gatherer. The server runs on a separate port from the user-facing listener
// so misconfigured ingress cannot accidentally publish /metrics. The returned
// server can be Shutdown by the caller during graceful drain.
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

// statusRecorder captures the HTTP status code so the metrics middleware can
// label the counter by response code. It explicitly delegates http.Hijacker,
// http.Flusher, and http.Pusher so wrapping does not silently break SSE,
// WebSocket upgrades, or HTTP/2 push if a route uses them.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// metricsMiddleware records request count, duration, and in-flight gauge for
// every HTTP request. Route template (low cardinality) is used as the handler
// label. Calls without a matched route fall back to "unmatched" so unknown
// paths cannot inflate label cardinality.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpInFlightRequests.Inc()
		defer httpInFlightRequests.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start).Seconds()

		handler := routeTemplate(r)
		httpRequestsTotal.WithLabelValues(handler, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(handler).Observe(elapsed)
	})
}

func routeTemplate(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if tpl, err := route.GetPathTemplate(); err == nil && tpl != "" {
			return tpl
		}
	}
	return "unmatched"
}
