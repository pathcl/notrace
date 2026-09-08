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
	"go.opentelemetry.io/otel"
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

func main() {
	prometheus.MustRegister(requestsTotal, requestDurationMs)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tp, err := initTracer(ctx)
	if err != nil {
		log.Fatalf("init tracer: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		tp.Shutdown(shutCtx) //nolint:errcheck
	}()

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
	go func() { defer wg.Done(); runFrontend(ctx) }()
	go func() { defer wg.Done(); runCheckout(ctx) }()
	go func() { defer wg.Done(); runBackground(ctx) }()

	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	metricsSrv.Shutdown(shutCtx) //nolint:errcheck
	wg.Wait()
}

func initTracer(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4318"
	}

	// Strip scheme if caller passed a full URL.
	if u, err := url.Parse("http://" + endpoint); err == nil && u.Host != "" {
		endpoint = u.Host
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceNameKey.String("traffic-gen")),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

// runFrontend simulates HTTP traffic to a product-browsing frontend service.
func runFrontend(ctx context.Context) {
	tracer := otel.Tracer("frontend")
	ops := []struct{ method, route string }{
		{"GET", "/api/products"},
		{"GET", "/api/users/{id}"},
		{"GET", "/api/recommendations"},
		{"POST", "/api/cart/items"},
	}
	for {
		op := ops[rand.Intn(len(ops))]
		simulateHTTPRequest(ctx, tracer, "frontend", op.method, op.route, func(ctx context.Context) {
			simulateChildSpan(ctx, tracer, "db", "SELECT products", jitter(5, 40))
			simulateChildSpan(ctx, tracer, "cache", "GET products:list", jitter(1, 5))
		})
		if !sleep(ctx, jitter(300*time.Millisecond, 1200*time.Millisecond)) {
			return
		}
	}
}

// runCheckout simulates order-placement traffic with deeper call chains.
func runCheckout(ctx context.Context) {
	tracer := otel.Tracer("checkout")
	ops := []struct{ method, route string }{
		{"POST", "/api/orders"},
		{"GET", "/api/orders/{id}"},
		{"POST", "/api/payments"},
	}
	for {
		op := ops[rand.Intn(len(ops))]
		simulateHTTPRequest(ctx, tracer, "checkout", op.method, op.route, func(ctx context.Context) {
			simulateChildSpan(ctx, tracer, "db", "INSERT orders", jitter(10, 60))
			simulateChildSpan(ctx, tracer, "payment-svc", "ChargeCard", jitter(50, 200))
			simulateChildSpan(ctx, tracer, "db", "UPDATE inventory", jitter(5, 20))
		})
		if !sleep(ctx, jitter(800*time.Millisecond, 2500*time.Millisecond)) {
			return
		}
	}
}

// runBackground simulates periodic background jobs (inventory sync, email, etc.).
func runBackground(ctx context.Context) {
	tracer := otel.Tracer("background-worker")
	jobs := []string{"sync-inventory", "send-notifications", "cleanup-sessions"}
	for {
		job := jobs[rand.Intn(len(jobs))]
		simulateJob(ctx, tracer, job)
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
			attribute.String("service.name", service),
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

func simulateChildSpan(ctx context.Context, tracer trace.Tracer, component, op string, d time.Duration) {
	_, span := tracer.Start(ctx, op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("component", component)),
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
