package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type retainedExporter struct{ *tracetest.InMemoryExporter }

// Keep flushed spans available for assertions after launcher shutdown.
func (retainedExporter) Shutdown(context.Context) error { return nil }

type notifyingProcessor struct {
	sdk.SpanProcessor
	started chan struct{}
	once    sync.Once
}

func (p *notifyingProcessor) OnStart(ctx context.Context, span sdk.ReadWriteSpan) {
	p.SpanProcessor.OnStart(ctx, span)
	if span.Name() == "Booking request" {
		p.once.Do(func() { close(p.started) })
	}
}

func TestExperimentDrainsAndExportsOnShutdown(t *testing.T) {
	exporter := retainedExporter{tracetest.NewInMemoryExporter()}
	processor := &notifyingProcessor{
		SpanProcessor: sdk.NewBatchSpanProcessor(exporter, sdk.WithBatchTimeout(time.Hour)),
		started:       make(chan struct{}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- run(ctx, listener, processor)
	}()
	go func() {
		select {
		case <-processor.started:
			cancel()
		case <-ctx.Done():
		}
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client := &http.Client{Timeout: 15 * time.Second}
	url := "http://" + listener.Addr().String() + "/api/experiment"
	for _, request := range []struct {
		header string
		body   string
		status int
	}{
		{"", "", http.StatusForbidden},
		{"run", "unexpected body", http.StatusBadRequest},
		{"run", "", http.StatusOK},
	} {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(request.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Trace-Lab", request.header)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != request.status {
			t.Fatalf("experiment returned %d, want %d", resp.StatusCode, request.status)
		}
		if request.status == http.StatusOK {
			var phases []phase
			if err := json.NewDecoder(resp.Body).Decode(&phases); err != nil {
				t.Fatal(err)
			}
			verifyExperiment(t, phases)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	verifyTraceChains(t, exporter.GetSpans())
}

func verifyTraceChains(t *testing.T, spans tracetest.SpanStubs) {
	t.Helper()
	expected := []struct {
		service string
		kind    trace.SpanKind
	}{
		{"workbench", trace.SpanKindInternal},
		{"workbench", trace.SpanKindClient},
		{"Booking", trace.SpanKindServer},
		{"Booking", trace.SpanKindClient},
		{"Availability", trace.SpanKindServer},
		{"Availability", trace.SpanKindClient},
		{"Confirmation", trace.SpanKindServer},
	}
	chains := make(map[trace.TraceID]map[trace.SpanID]tracetest.SpanStub)
	for _, span := range spans {
		id := span.SpanContext.TraceID()
		if chains[id] == nil {
			chains[id] = make(map[trace.SpanID]tracetest.SpanStub)
		}
		chains[id][span.Parent.SpanID()] = span
	}
	if len(chains) != 3*sampleCount || len(spans) != 3*sampleCount*len(expected) {
		t.Fatalf("got %d trace IDs and %d spans", len(chains), len(spans))
	}
	for id, chain := range chains {
		var parent trace.SpanID
		for _, want := range expected {
			span, ok := chain[parent]
			if !ok {
				t.Fatalf("trace %s has no child of %s", id, parent)
			}
			service, _ := span.Resource.Set().Value("service.name")
			if service.AsString() != want.service || span.SpanKind != want.kind {
				t.Fatalf("trace %s after %s: got %s %s, want %s %s", id, parent, service.AsString(), span.SpanKind, want.service, want.kind)
			}
			if parent.IsValid() && span.Parent.TraceID() != id {
				t.Fatalf("trace %s has a parent in trace %s", id, span.Parent.TraceID())
			}
			parent = span.SpanContext.SpanID()
		}
	}
}

func verifyExperiment(t *testing.T, phases []phase) {
	t.Helper()
	if len(phases) != 3 {
		t.Fatalf("got %d phases", len(phases))
	}
	traceIDs := make(map[string]bool)
	for index, id := range []string{"baseline", "slowdown", "recovery"} {
		phase := phases[index]
		if phase.ID != id || len(phase.Samples) != sampleCount {
			t.Fatalf("invalid phase %s with %d requests", phase.ID, len(phase.Samples))
		}
		for _, sample := range phase.Samples {
			if len(sample.TraceID) != 32 || traceIDs[sample.TraceID] || len(sample.Spans) != 3 {
				t.Fatalf("invalid sample %+v", sample)
			}
			traceIDs[sample.TraceID] = true
			for i, name := range []string{"Booking", "Availability", "Confirmation"} {
				span := sample.Spans[i]
				if span.Service != name || span.Duration <= 0 || len(span.ID) != 16 || span.Parent == "0000000000000000" {
					t.Fatalf("invalid span %+v", span)
				}
			}
			if id == "slowdown" && sample.Spans[1].Duration < ms(slowDelay) {
				t.Fatalf("Availability did not exhibit the configured delay: %+v", sample)
			}
		}
	}
}

func TestStats(t *testing.T) {
	p50, p95 := stats([]sample{{Elapsed: 9}, {Elapsed: 3}, {Elapsed: 15}, {Elapsed: 1}})
	if p50 != 6 || p95 != 15 {
		t.Fatalf("got median %v and p95 %v", p50, p95)
	}
}

func TestAvailabilityDelay(t *testing.T) {
	for phase, want := range map[string]time.Duration{
		"baseline": 12 * time.Millisecond,
		"slowdown": 192 * time.Millisecond,
		"recovery": 12 * time.Millisecond,
	} {
		if got := availabilityDelay(phase); got != want {
			t.Errorf("%s delay = %s, want %s", phase, got, want)
		}
	}
}

func TestInternalRouteRejectsBeforeTracing(t *testing.T) {
	const credential = "test-credential"
	recorder := tracetest.NewSpanRecorder()
	provider := sdk.NewTracerProvider(sdk.WithSpanProcessor(recorder))
	traced := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "internal", otelhttp.WithTracerProvider(provider))
	handler := internalRoute(credential, traced)

	for _, tc := range []struct {
		path, header string
		status       int
	}{
		{"/", "", http.StatusForbidden},
		{"/", "incorrect", http.StatusForbidden},
		{"/extra", credential, http.StatusNotFound},
		{"/", credential, http.StatusNoContent},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set(internalHeader, tc.header)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.path, w.Code, tc.status)
		}
	}
	if got := len(recorder.Started()); got != 1 {
		t.Fatalf("started %d spans, want 1", got)
	}
}
