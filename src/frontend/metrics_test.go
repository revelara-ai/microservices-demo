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
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// TestMetricsMiddleware_Counts200And500 ensures the counter is labeled by
// status code and the histogram observes a sample per request, using the
// route template (not the raw URL) as the handler label.
func TestMetricsMiddleware_Counts200And500(t *testing.T) {
	r := mux.NewRouter()
	r.Use(metricsMiddleware)
	r.HandleFunc("/product/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods(http.MethodGet)
	r.HandleFunc("/cart", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}).Methods(http.MethodGet)

	startOK := counterValue(t, httpRequestsTotal.WithLabelValues("/product/{id}", "200").(prometheus.Counter))
	start500 := counterValue(t, httpRequestsTotal.WithLabelValues("/cart", "500").(prometheus.Counter))
	startHist := histogramSampleCount(t, httpRequestDuration.WithLabelValues("/product/{id}").(prometheus.Histogram))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/product/abc123", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cart", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	if got := counterValue(t, httpRequestsTotal.WithLabelValues("/product/{id}", "200").(prometheus.Counter)); got != startOK+1 {
		t.Errorf("200 counter: got %v, want %v", got, startOK+1)
	}
	if got := counterValue(t, httpRequestsTotal.WithLabelValues("/cart", "500").(prometheus.Counter)); got != start500+1 {
		t.Errorf("500 counter: got %v, want %v", got, start500+1)
	}
	if got := histogramSampleCount(t, httpRequestDuration.WithLabelValues("/product/{id}").(prometheus.Histogram)); got != startHist+1 {
		t.Errorf("histogram samples: got %v, want %v", got, startHist+1)
	}
}

// TestMetricsMiddleware_UnmatchedRoute verifies requests that don't match
// any mux route still record metrics under the bounded "unmatched" label
// rather than inflating cardinality with raw paths.
func TestMetricsMiddleware_UnmatchedRoute(t *testing.T) {
	mw := metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	start := counterValue(t, httpRequestsTotal.WithLabelValues("unmatched", "404").(prometheus.Counter))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))

	if got := counterValue(t, httpRequestsTotal.WithLabelValues("unmatched", "404").(prometheus.Counter)); got != start+1 {
		t.Errorf("unmatched counter: got %v, want %v", got, start+1)
	}
}

// TestCircuitBreakerStateChangeCounter verifies the gobreaker OnStateChange
// callback (configured in newCircuitBreaker) emits a Prometheus counter so
// SLO burn-rate alerts can fire on circuit open/close events.
func TestCircuitBreakerStateChangeCounter(t *testing.T) {
	circuitBreakerStateChanges.Reset()
	circuitBreakerStateChanges.WithLabelValues("test-circuit", "open").Inc()
	circuitBreakerStateChanges.WithLabelValues("test-circuit", "open").Inc()
	circuitBreakerStateChanges.WithLabelValues("test-circuit", "closed").Inc()

	if got := counterValue(t, circuitBreakerStateChanges.WithLabelValues("test-circuit", "open").(prometheus.Counter)); got != 2 {
		t.Errorf("open counter: got %v, want 2", got)
	}
	if got := counterValue(t, circuitBreakerStateChanges.WithLabelValues("test-circuit", "closed").(prometheus.Counter)); got != 1 {
		t.Errorf("closed counter: got %v, want 1", got)
	}
}
