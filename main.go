package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

//go:embed web/*
var assets embed.FS

const sampleCount = 9
const slowDelay = 180 * time.Millisecond
const experimentTimeout = 15 * time.Second
const internalHeader = "X-Trace-Lab-Internal"

var propagator = propagation.TraceContext{}

type spanRow struct {
	Service  string  `json:"service"`
	ID       string  `json:"id"`
	Parent   string  `json:"parent"`
	Offset   float64 `json:"offset"`
	Duration float64 `json:"duration"`
}

type sample struct {
	Elapsed float64   `json:"elapsed"`
	TraceID string    `json:"traceId"`
	Spans   []spanRow `json:"spans"`
}

type phase struct {
	ID      string   `json:"id"`
	P50     float64  `json:"p50"`
	P95     float64  `json:"p95"`
	Samples []sample `json:"samples"`
}

type capture struct {
	mu      sync.Mutex
	spans   map[trace.TraceID][]sdk.ReadOnlySpan
	changed chan struct{}
}

func (c *capture) OnStart(context.Context, sdk.ReadWriteSpan) {}
func (c *capture) Shutdown(context.Context) error             { return nil }
func (c *capture) ForceFlush(context.Context) error           { return nil }

func (c *capture) OnEnd(span sdk.ReadOnlySpan) {
	if span.SpanKind() != trace.SpanKindServer {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := span.SpanContext().TraceID()
	if rows, exists := c.spans[id]; exists && len(rows) < 3 {
		c.spans[id] = append(rows, span)
		close(c.changed)
		c.changed = make(chan struct{})
	}
}

func (c *capture) collect(ctx context.Context, id trace.TraceID) ([]spanRow, error) {
	for {
		c.mu.Lock()
		spans, changed := c.spans[id], c.changed
		c.mu.Unlock()
		if len(spans) == 3 {
			slices.SortFunc(spans, func(a, b sdk.ReadOnlySpan) int { return a.StartTime().Compare(b.StartTime()) })
			rows := make([]spanRow, 0, len(spans))
			for _, s := range spans {
				service, _ := s.Resource().Set().Value(attribute.Key("service.name"))
				rows = append(rows, spanRow{service.AsString(), s.SpanContext().SpanID().String(), s.Parent().SpanID().String(),
					ms(s.StartTime().Sub(spans[0].StartTime())), ms(s.EndTime().Sub(s.StartTime()))})
			}
			return rows, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func stats(samples []sample) (float64, float64) {
	times := make([]float64, 0, len(samples))
	for _, item := range samples {
		times = append(times, item.Elapsed)
	}
	slices.Sort(times)
	n := len(times)
	median := (times[(n-1)/2] + times[n/2]) / 2
	return median, times[(95*n+99)/100-1]
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func call(ctx context.Context, client *http.Client, url, credential string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(internalHeader, credential)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return nil
}

type lab struct {
	capture    *capture
	tracer     trace.Tracer
	client     *http.Client
	booking    string
	credential string
	running    atomic.Bool
	delay      atomic.Int64
}

func (l *lab) measure(ctx context.Context) (sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ctx, root := l.tracer.Start(ctx, "Booking request")
	defer root.End()
	id := root.SpanContext().TraceID()
	l.capture.mu.Lock()
	l.capture.spans[id] = nil
	l.capture.mu.Unlock()
	start := time.Now()
	if err := call(ctx, l.client, l.booking, l.credential); err != nil {
		return sample{}, err
	}
	elapsed := ms(time.Since(start))
	rows, err := l.capture.collect(ctx, id)
	return sample{elapsed, id.String(), rows}, err
}

func (l *lab) experiment(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Trace-Lab") != "run" {
		http.Error(w, "Missing experiment header", http.StatusForbidden)
		return
	}
	if r.ContentLength != 0 {
		http.Error(w, "The experiment request must have an empty body", http.StatusBadRequest)
		return
	}
	if !l.running.CompareAndSwap(false, true) {
		http.Error(w, "An experiment is already running", http.StatusConflict)
		return
	}
	l.capture.mu.Lock()
	l.capture.spans = make(map[trace.TraceID][]sdk.ReadOnlySpan)
	l.capture.changed = make(chan struct{})
	l.capture.mu.Unlock()
	defer func() {
		l.delay.Store(0)
		l.capture.mu.Lock()
		l.capture.spans = nil
		l.capture.mu.Unlock()
		l.running.Store(false)
	}()
	ctx, cancel := context.WithTimeout(r.Context(), experimentTimeout)
	defer cancel()
	result := make([]phase, 0, 3)
	for _, id := range []string{"baseline", "slowdown", "recovery"} {
		l.delay.Store(int64(availabilityDelay(id)))
		p := phase{ID: id}
		for range sampleCount {
			item, err := l.measure(ctx)
			if err != nil {
				log.Printf("experiment %s: %v", id, err)
				http.Error(w, "A service request failed. Try running the experiment again.", http.StatusBadGateway)
				return
			}
			p.Samples = append(p.Samples, item)
		}
		p.P50, p.P95 = stats(p.Samples)
		result = append(result, p)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("write experiment: %v", err)
	}
}

func availabilityDelay(phase string) time.Duration {
	if phase == "slowdown" {
		return 12*time.Millisecond + slowDelay
	}
	return 12 * time.Millisecond
}

func internalRoute(credential string, next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(internalHeader)), []byte(credential)) != 1 {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
	return mux
}

func run(ctx context.Context, listener net.Listener, processor sdk.SpanProcessor) error {
	defer listener.Close()

	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("generate internal credential: %w", err)
	}

	var servers []*http.Server
	var providers []*sdk.TracerProvider
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		for _, server := range servers {
			_ = server.Close()
		}
		for _, provider := range providers {
			if err := provider.Shutdown(stop); err != nil {
				log.Printf("flush traces: %v", err)
			}
		}
		if processor != nil {
			flush, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := processor.Shutdown(flush); err != nil {
				log.Printf("flush trace exporter: %v", err)
			}
		}
	}()
	l := &lab{capture: &capture{}, credential: hex.EncodeToString(secret[:])}
	provider := func(service string) *sdk.TracerProvider {
		options := []sdk.TracerProviderOption{
			sdk.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
			sdk.WithSpanProcessor(l.capture),
		}
		if processor != nil {
			options = append(options, sdk.WithSpanProcessor(forwardSpans{processor}))
		}
		p := sdk.NewTracerProvider(options...)
		providers = append(providers, p)
		return p
	}
	uiProvider := provider("workbench")
	l.tracer = uiProvider.Tracer("trace-lab")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := func(p *sdk.TracerProvider) *http.Client {
		return &http.Client{
			Timeout: 2 * time.Second,
			Transport: otelhttp.NewTransport(transport,
				otelhttp.WithTracerProvider(p), otelhttp.WithPropagators(propagator)),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	l.client = client(uiProvider)
	startService := func(name, downstream string, delay func() time.Duration) (string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		p := provider(name)
		outgoing := client(p)
		handler := internalRoute(l.credential, otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !pause(r.Context(), delay()) {
				return
			}
			if downstream != "" {
				if err := call(r.Context(), outgoing, downstream, l.credential); err != nil {
					http.Error(w, "Downstream unavailable", http.StatusBadGateway)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}), name, otelhttp.WithTracerProvider(p), otelhttp.WithPropagators(propagator)))
		server := &http.Server{Handler: handler, ReadTimeout: 2 * time.Second, IdleTimeout: 30 * time.Second}
		servers = append(servers, server)
		go func() {
			if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("%s: %v", name, err)
			}
		}()
		return "http://" + ln.Addr().String(), nil
	}
	confirmation, err := startService("Confirmation", "", func() time.Duration { return 6 * time.Millisecond })
	if err != nil {
		return err
	}
	availability, err := startService("Availability", confirmation, func() time.Duration {
		return time.Duration(l.delay.Load())
	})
	if err != nil {
		return err
	}
	l.booking, err = startService("Booking", availability, func() time.Duration { return 5 * time.Millisecond })
	if err != nil {
		return err
	}

	web, err := fs.Sub(assets, "web")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/experiment", l.experiment)
	mux.Handle("GET /", http.FileServerFS(web))
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := listener.Addr().String()
		_, port, _ := net.SplitHostPort(host)
		if r.Host != host && r.Host != net.JoinHostPort("localhost", port) {
			http.Error(w, "Invalid host", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	}), ReadTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 20 * time.Second}
	servers = append(servers, server)
	served := make(chan error, 1)
	log.Printf("Trace Lab ready at http://%s", listener.Addr())
	go func() { served <- server.Serve(listener) }()
	select {
	case err = <-served:
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), experimentTimeout+time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(stop); shutdownErr != nil {
			log.Printf("workbench shutdown: %v", shutdownErr)
		}
		err = <-served
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Providers share the batch queue. The launcher owns its shutdown.
type forwardSpans struct{ sdk.SpanProcessor }

func (forwardSpans) Shutdown(context.Context) error { return nil }

func main() {
	port := flag.Int("port", 8080, "loopback web port")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	var processor sdk.SpanProcessor
	if os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		// Keep export available while the workbench drains after a signal.
		exporter, err := otlptracehttp.New(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		processor = sdk.NewBatchSpanProcessor(exporter)
	}
	if err := run(ctx, listener, processor); err != nil {
		log.Fatal(err)
	}
}
