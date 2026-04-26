//go:build e2e

package postgrestest

import (
	"context"
	"database/sql"
	"log"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/loewenthal-corp/consensus/internal/postgres"
	"github.com/loewenthal-corp/consensus/internal/search"
)

type TestDB struct {
	*postgres.Client
	Pool  *pgxpool.Pool
	SQLDB *sql.DB
}

func New(ctx context.Context, t *testing.T) *TestDB {
	t.Helper()

	container, err := postgrescontainer.Run(ctx,
		"timescale/timescaledb-ha:pg17",
		postgrescontainer.WithDatabase("testdb"),
		postgrescontainer.WithUsername("postgres"),
		postgrescontainer.WithPassword("postgres"),
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "shared_preload_libraries=timescaledb,pg_textsearch"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			log.Printf("failed to terminate postgres container: %s", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { pool.Close() })

	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	client := postgres.NewClient(postgres.Driver(drv))
	t.Cleanup(func() { client.Close() })

	require.NoError(t, client.Schema.Create(ctx))
	require.NoError(t, search.EnsureSchema(ctx, sqlDB))

	return &TestDB{Client: client, Pool: pool, SQLDB: sqlDB}
}
