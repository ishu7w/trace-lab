# Trace Lab

Run a controlled latency incident against three instrumented Go HTTP services. Compare baseline, slowdown, and recovery from live measurements, then inspect the server spans behind any sampled request.

## Run locally

Requires Go 1.26 or later. The first run downloads Go dependencies. After that, the workbench runs locally without an API key or paid service.

```sh
go run .
```

Open [Trace Lab](http://127.0.0.1:8080) and choose **Run experiment**. Use `go run . -port 8090` if port 8080 is in use.

1. Compare per-phase p95 and median across nine sequential requests.
2. Inspect the slowdown trace. Availability adds 180 ms before calling Confirmation, so the slowdown propagates into Booking's inclusive duration.
3. Switch to recovery to inspect the same service chain after the delay is removed. Choose any request in the phase selector's companion request menu.

The chart uses measured server results. Request latency is measured by the Go workbench client and the waterfall shows observed server spans from the OpenTelemetry SDK. Parent IDs reference the full trace, which includes outgoing HTTP client spans. With nine samples the nearest-rank p95 is the maximum, so the workbench is intended for demonstration. The three HTTP servers share one local Go process.

## Optional Jaeger

With Docker running, start Jaeger before the Go app.

```sh
docker compose up -d
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:4318/v1/traces go run .
```

Run an experiment, copy a trace ID from the inspector, and open it in [Jaeger](http://127.0.0.1:16686). Batch delivery can take a few seconds. Use `docker compose down` to stop Jaeger.

## Verify

```sh
go test -race ./...
go vet ./...
go build ./...
docker compose config
```

See [the implementation plan](docs/PLAN.md), [architecture and security model](docs/ARCHITECTURE.md), and [workbench design](docs/DESIGN.md).
