package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alecthomas/kong"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/loewenthal-corp/consensus/internal/buildinfo"
	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	"github.com/loewenthal-corp/consensus/internal/postgres"
	"github.com/loewenthal-corp/consensus/internal/search"
	"github.com/loewenthal-corp/consensus/internal/server"
)

type CLI struct {
	Port        string `help:"API/admin port to listen on." env:"PORT" default:"8080"`
	MCPPort     string `help:"MCP port to listen on." env:"MCP_PORT" default:"8081"`
	DatabaseURL string `help:"Postgres connection string." env:"DATABASE_URL" default:"postgres://postgres:postgres@localhost:5432/consensus?sslmode=disable"`
	Migrate     bool   `help:"Run Ent migrations at startup." env:"MIGRATE" default:"true" negatable:""`
	LogLevel    string `help:"Log level: debug, info, warn, error." env:"LOG_LEVEL" default:"info"`
	Debug       bool   `help:"Enable verbose debug logging. Equivalent to LOG_LEVEL=debug." env:"DEBUG" default:"false"`
}

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("consensus"),
		kong.Description("Consensus server"),
		kong.UsageOnError(),
	)

	setupLogging(cli)
	shutdownTracing, err := setupTracing(context.Background())
	if err != nil {
		slog.Error("failed to setup tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			slog.Error("failed to shutdown tracing", "error", err)
		}
	}()

	if err := run(context.Background(), cli); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func setupLogging(cfg CLI) {
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	} else if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})))
}

func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String("consensus"),
		semconv.ServiceVersionKey.String(buildinfo.Version),
	}
	if buildinfo.Commit != "" && buildinfo.Commit != "unknown" {
		attrs = append(attrs, attribute.String("service.commit", buildinfo.Commit))
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	tracerOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create otel trace exporter: %w", err)
		}
		tracerOptions = append(tracerOptions, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(tracerOptions...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider.Shutdown, nil
}

func run(ctx context.Context, cfg CLI) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close()

	entClient := postgres.NewClient(postgres.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer entClient.Close()

	if cfg.Migrate {
		if err := entClient.Schema.Create(ctx); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
		if err := search.EnsureSchema(ctx, sqlDB); err != nil {
			return fmt.Errorf("prepare search schema: %w", err)
		}
	}

	svc := consensus.NewService(entClient)

	apiHandler, err := server.NewAPI(server.Config{
		Service: svc,
	})
	if err != nil {
		return err
	}
	mcpHandler, err := server.NewMCP(server.Config{
		Service: svc,
	})
	if err != nil {
		return err
	}

	apiServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           apiHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	mcpServer := &http.Server{
		Addr:              ":" + cfg.MCPPort,
		Handler:           mcpHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}

	errCh := make(chan error, 2)
	startServer := func(name, port string, srv *http.Server) {
		go func() {
			slog.Info("starting consensus "+name+" server", "port", port)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
	}
	startServer("api", cfg.Port, apiServer)
	startServer("mcp", cfg.MCPPort, mcpServer)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		runErr = err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := errors.Join(
		apiServer.Shutdown(shutdownCtx),
		mcpServer.Shutdown(shutdownCtx),
	)
	if runErr != nil {
		if shutdownErr != nil {
			return errors.Join(runErr, shutdownErr)
		}
		return runErr
	}
	return shutdownErr
}
