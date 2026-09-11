package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "traffic_gen_requests_total",
		Help: "Total simulated requests by service, operation, and status.",
	}, []string{"service", "operation", "status"})

	requestDurationMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "traffic_gen_request_duration_milliseconds",
		Help:    "Simulated request duration in milliseconds.",
		Buckets: []float64{5, 10, 25, 50, 100, 250, 500, 1000},
	}, []string{"service", "operation"})
)

// tracers holds one tracer per logical service. Each tracer is backed by its
// own TracerProvider with the correct service.name resource, so spans are
// exported in separate OTLP batches — matching a real multi-service deployment.
type tracers struct {
	frontend   trace.Tracer
	checkout   trace.Tracer
	paymentSvc trace.Tracer
	db         trace.Tracer
	cache      trace.Tracer
	bgWorker   trace.Tracer
}

func main() {
	prometheus.MustRegister(requestsTotal, requestDurationMs)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	endpoint := resolveEndpoint(os.Getenv("OTLP_ENDPOINT"))

	serviceNames := []string{"frontend", "checkout", "payment-svc", "db", "cache", "background-worker"}
	providers := make([]*sdktrace.TracerProvider, 0, len(serviceNames))
	providerMap := make(map[string]*sdktrace.TracerProvider, len(serviceNames))
	for _, name := range serviceNames {
		tp, err := newTracerProvider(ctx, name, endpoint)
		if err != nil {
			log.Fatalf("init tracer for %s: %v", name, err)
		}
		providers = append(providers, tp)
		providerMap[name] = tp
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		for _, tp := range providers {
			tp.Shutdown(shutCtx) //nolint:errcheck
		}
	}()

	t := tracers{
		frontend:   providerMap["frontend"].Tracer("frontend"),
		checkout:   providerMap["checkout"].Tracer("checkout"),
		paymentSvc: providerMap["payment-svc"].Tracer("payment-svc"),
		db:         providerMap["db"].Tracer("db"),
		cache:      providerMap["cache"].Tracer("cache"),
		bgWorker:   providerMap["background-worker"].Tracer("background-worker"),
	}

	addr := os.Getenv("PROMETHEUS_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	metricsSrv := &http.Server{
		Addr:    addr,
		Handler: promhttp.Handler(),
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server error: %v", err)
		}
	}()

	if readyURL := os.Getenv("TEMPO_READY_URL"); readyURL != "" {
		log.Printf("waiting for Tempo at %s", readyURL)
		if err := waitForTempo(ctx, readyURL); err != nil {
			log.Fatalf("tempo never became ready: %v", err)
		}
		log.Printf("Tempo is ready")
	}

	log.Printf("traffic-gen started — metrics on %s", addr)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); runFrontend(ctx, t) }()
	go func() { defer wg.Done(); runCheckout(ctx, t) }()
	go func() { defer wg.Done(); runBackground(ctx, t) }()

	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	metricsSrv.Shutdown(shutCtx) //nolint:errcheck
	wg.Wait()
}

// newTracerProvider creates a TracerProvider for a single service, exporting
// to the given OTLP HTTP endpoint. Each provider sets service.name on its
// resource so spans appear as separate services in Tempo and ClickHouse.
func newTracerProvider(ctx context.Context, serviceName, endpoint string) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	), nil
}

func resolveEndpoint(raw string) string {
	if raw == "" {
		return "localhost:4318"
	}
	if u, err := url.Parse("http://" + raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// runFrontend simulates HTTP traffic to a product-browsing frontend service.
func runFrontend(ctx context.Context, t tracers) {
	ops := []struct{ method, route string }{
		{"GET", "/api/products"},
		{"GET", "/api/users/{id}"},
		{"GET", "/api/recommendations"},
		{"POST", "/api/cart/items"},
	}
	for {
		op := ops[rand.Intn(len(ops))]
		simulateHTTPRequest(ctx, t.frontend, "frontend", op.method, op.route, func(ctx context.Context) {
			simulateChildSpan(ctx, t.db, "SELECT products", jitter(5, 40))
			simulateChildSpan(ctx, t.cache, "GET products:list", jitter(1, 5))
		})
		if !sleep(ctx, jitter(300*time.Millisecond, 1200*time.Millisecond)) {
			return
		}
	}
}

// runCheckout simulates order-placement traffic with deeper call chains.
func runCheckout(ctx context.Context, t tracers) {
	ops := []struct{ method, route string }{
		{"POST", "/api/orders"},
		{"GET", "/api/orders/{id}"},
		{"POST", "/api/payments"},
	}
	for {
		op := ops[rand.Intn(len(ops))]
		simulateHTTPRequest(ctx, t.checkout, "checkout", op.method, op.route, func(ctx context.Context) {
			simulateChildSpan(ctx, t.db, "INSERT orders", jitter(10, 60))
			simulateChildSpan(ctx, t.paymentSvc, "ChargeCard", jitter(50, 200))
			simulateChildSpan(ctx, t.db, "UPDATE inventory", jitter(5, 20))
		})
		if !sleep(ctx, jitter(800*time.Millisecond, 2500*time.Millisecond)) {
			return
		}
	}
}

// runBackground simulates periodic background jobs (inventory sync, email, etc.).
func runBackground(ctx context.Context, t tracers) {
	jobs := []string{"sync-inventory", "send-notifications", "cleanup-sessions"}
	for {
		job := jobs[rand.Intn(len(jobs))]
		simulateJob(ctx, t.bgWorker, job)
		if !sleep(ctx, jitter(2*time.Second, 5*time.Second)) {
			return
		}
	}
}

func simulateHTTPRequest(ctx context.Context, tracer trace.Tracer, service, method, route string, children func(context.Context)) {
	isError := rand.Float64() < 0.15
	start := time.Now()

	ctx, span := tracer.Start(ctx, method+" "+route,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.method", method),
			attribute.String("http.route", route),
		),
	)
	defer span.End()

	if children != nil {
		children(ctx)
	}
	time.Sleep(jitter(10*time.Millisecond, 80*time.Millisecond))

	status := "ok"
	if isError {
		status = "error"
		span.SetStatus(codes.Error, "internal server error")
		span.SetAttributes(attribute.Int("http.status_code", 500))
	} else {
		span.SetAttributes(attribute.Int("http.status_code", 200))
	}

	dur := float64(time.Since(start).Milliseconds())
	requestsTotal.WithLabelValues(service, method+" "+route, status).Inc()
	requestDurationMs.WithLabelValues(service, method+" "+route).Observe(dur)
}

// simulateChildSpan starts a client span using the child service's own tracer.
// The ctx carries the parent span, so parentSpanId is set automatically by the
// OTel SDK — no explicit propagation needed when running in-process.
func simulateChildSpan(ctx context.Context, tracer trace.Tracer, op string, d time.Duration) {
	_, span := tracer.Start(ctx, op,
		trace.WithSpanKind(trace.SpanKindClient),
	)
	defer span.End()
	time.Sleep(d)
}

func simulateJob(ctx context.Context, tracer trace.Tracer, job string) {
	_, span := tracer.Start(ctx, job,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("job.name", job)),
	)
	defer span.End()
	time.Sleep(jitter(100*time.Millisecond, 500*time.Millisecond))
}

// waitForTempo polls readyURL every 2s until it returns HTTP 200 or ctx is cancelled.
func waitForTempo(ctx context.Context, readyURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			resp, err := client.Get(readyURL)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func jitter(min, max time.Duration) time.Duration {
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
