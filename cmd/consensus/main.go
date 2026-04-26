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

	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	"github.com/loewenthal-corp/consensus/internal/postgres"
	"github.com/loewenthal-corp/consensus/internal/server"
)

type CLI struct {
	Port        string `help:"API/admin port to listen on." env:"PORT" default:"8080"`
	MCPPort     string `help:"MCP port to listen on." env:"MCP_PORT" default:"8081"`
	DatabaseURL string `help:"Postgres connection string." env:"DATABASE_URL" default:"postgres://postgres:postgres@localhost:5432/consensus?sslmode=disable"`
	Migrate     bool   `help:"Run Ent migrations at startup." env:"MIGRATE" default:"true" negatable:""`
}

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("consensus"),
		kong.Description("Consensus server"),
		kong.UsageOnError(),
	)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(context.Background(), cli); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
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
