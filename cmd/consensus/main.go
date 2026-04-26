package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	Port        string `help:"Port to listen on." env:"PORT" default:"8080"`
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
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
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

	handler, err := server.New(server.Config{
		Service: consensus.NewService(entClient),
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}

	go func() {
		slog.Info("starting consensus server", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
