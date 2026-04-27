package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/loewenthal-corp/consensus/internal/postgres"
	insighttags "github.com/loewenthal-corp/consensus/internal/tags"
)

const (
	defaultCandidateLimit = 30
	maxCandidateLimit     = 100
	maxReturnLimit        = 50
)

type PostgresSearcher struct {
	db *sql.DB

	schemaMu    sync.Mutex
	schemaReady bool
}

func NewPostgresSearcher(db *sql.DB) *PostgresSearcher {
	return &PostgresSearcher{db: db}
}

func (s *PostgresSearcher) Search(ctx context.Context, req Request) ([]Result, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("search database is not configured")
	}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}

	tenantKey := strings.TrimSpace(req.TenantKey)
	if tenantKey == "" {
		tenantKey = "default"
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, errors.New("query is required")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > maxReturnLimit {
		limit = maxReturnLimit
	}
	candidateLimit := limit * 6
	if candidateLimit < defaultCandidateLimit {
		candidateLimit = defaultCandidateLimit
	}
	if candidateLimit > maxCandidateLimit {
		candidateLimit = maxCandidateLimit
	}

	tags := insighttags.NormalizeList(req.Tags)
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("marshal tag filters: %w", err)
	}
	filterSignals := make([]string, 0, 1)
	if len(tags) > 0 {
		filterSignals = append(filterSignals, "tag")
	}

	rows, err := s.db.QueryContext(ctx, searchSQL, tenantKey, query, candidateLimit, limit, string(tagsJSON))
	if err != nil {
		return nil, fmt.Errorf("run bm25 search: %w", err)
	}
	defer rows.Close()

	results := make([]Result, 0, limit)
	for rows.Next() {
		var row searchRow
		if err := rows.Scan(
			&row.insightID,
			&row.score,
			&row.bm25Rank,
			&row.bm25Score,
			&row.positive,
			&row.negative,
			&row.chunkKind,
		); err != nil {
			return nil, fmt.Errorf("scan bm25 search result: %w", err)
		}
		results = append(results, row.result(filterSignals))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bm25 search results: %w", err)
	}
	return results, nil
}

func (s *PostgresSearcher) IndexInsight(ctx context.Context, item *postgres.Insight) error {
	if s == nil || s.db == nil || item == nil {
		return nil
	}
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}

	chunks := buildInsightChunks(item)
	if len(chunks) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search chunk update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.SearchDocument) == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, upsertChunkSQL,
			uuid.New(),
			item.TenantKey,
			item.ID,
			chunk.Kind,
			chunk.Ordinal,
			chunk.Text,
			chunk.SearchDocument,
			chunk.ContentHash,
		)
		if err != nil {
			return fmt.Errorf("upsert search chunk: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search chunk update: %w", err)
	}
	return nil
}

func (s *PostgresSearcher) ensureSchema(ctx context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaReady {
		return nil
	}
	if err := EnsureSchema(ctx, s.db); err != nil {
		return err
	}
	s.schemaReady = true
	return nil
}

func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("search database is not configured")
	}
	for _, statement := range []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE EXTENSION IF NOT EXISTS pg_textsearch`,
		createChunksTableSQL,
		`CREATE INDEX IF NOT EXISTS insight_search_chunks_scope_idx ON insight_search_chunks (tenant_key, insight_id)`,
		`CREATE INDEX IF NOT EXISTS insight_search_chunks_bm25_idx ON insight_search_chunks USING bm25(search_document) WITH (text_config='english')`,
		backfillChunksSQL,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("prepare search schema: %w", err)
		}
	}
	return nil
}

type searchRow struct {
	insightID uuid.UUID
	score     float64
	bm25Rank  int
	bm25Score float64
	positive  float64
	negative  float64
	chunkKind string
}

func (r searchRow) result(filterSignals []string) Result {
	signals := append([]string{"bm25"}, filterSignals...)
	reason := "ranked highly by BM25"
	if len(filterSignals) > 0 {
		reason += " within requested filters"
	}
	if r.positive > 0 || r.negative > 0 {
		signals = append(signals, "outcome")
	}
	if r.positive > 0 && r.negative > 0 {
		reason += " with mixed outcome history"
	} else if r.positive > 0 {
		reason += " and has solved/helped outcomes"
	} else if r.negative > 0 {
		reason += "; penalized by negative outcomes"
	}
	return Result{
		InsightID:      r.insightID,
		Score:          math.Round(r.score*1_000_000) / 1_000_000,
		RankReason:     reason,
		MatchedSignals: signals,
	}
}

const createChunksTableSQL = `
CREATE TABLE IF NOT EXISTS insight_search_chunks (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_key text NOT NULL,
	insight_id uuid NOT NULL,
	chunk_kind text NOT NULL,
	chunk_ordinal integer NOT NULL,
	chunk_text text NOT NULL,
	search_document text NOT NULL,
	content_hash text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT insight_search_chunks_insight_chunk_key UNIQUE (tenant_key, insight_id, chunk_kind, chunk_ordinal)
)`

const backfillChunksSQL = `
WITH source AS (
	SELECT
		tenant_key,
		id AS insight_id,
		'main'::text AS chunk_kind,
		0 AS chunk_ordinal,
		concat_ws(E'\n\n',
			nullif(title, ''),
			nullif(problem, ''),
			nullif(answer, ''),
			nullif(action, ''),
			nullif(detail, ''),
			nullif(example->>'command', ''),
			nullif(example->>'content', ''),
			nullif(example->>'description', '')
		) AS chunk_text,
		concat_ws(E'\n',
			nullif(title, ''), nullif(title, ''), nullif(title, ''), nullif(title, ''),
			nullif(problem, ''), nullif(problem, ''), nullif(problem, ''), nullif(problem, ''),
			nullif(example->>'command', ''), nullif(example->>'command', ''), nullif(example->>'command', ''),
			nullif(example->>'content', ''), nullif(example->>'content', ''), nullif(example->>'content', ''),
			nullif(example->>'description', ''), nullif(example->>'description', ''), nullif(example->>'description', ''),
			nullif(answer, ''), nullif(answer, ''),
			nullif(action, ''), nullif(action, ''),
			nullif(detail, '')
		) AS search_document
	FROM insights
)
INSERT INTO insight_search_chunks (
	id,
	tenant_key,
	insight_id,
	chunk_kind,
	chunk_ordinal,
	chunk_text,
	search_document,
	content_hash
)
SELECT
	gen_random_uuid(),
	tenant_key,
	insight_id,
	chunk_kind,
	chunk_ordinal,
	chunk_text,
	search_document,
	md5(search_document)
FROM source
WHERE search_document <> ''
ON CONFLICT (tenant_key, insight_id, chunk_kind, chunk_ordinal) DO UPDATE SET
	chunk_text = EXCLUDED.chunk_text,
	search_document = EXCLUDED.search_document,
	content_hash = EXCLUDED.content_hash,
	updated_at = now()
WHERE insight_search_chunks.content_hash <> EXCLUDED.content_hash`

const upsertChunkSQL = `
INSERT INTO insight_search_chunks (
	id,
	tenant_key,
	insight_id,
	chunk_kind,
	chunk_ordinal,
	chunk_text,
	search_document,
	content_hash
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_key, insight_id, chunk_kind, chunk_ordinal) DO UPDATE SET
	chunk_text = EXCLUDED.chunk_text,
	search_document = EXCLUDED.search_document,
	content_hash = EXCLUDED.content_hash,
	updated_at = now()
WHERE insight_search_chunks.content_hash <> EXCLUDED.content_hash`

const searchSQL = `
WITH ranked_chunks AS (
	SELECT
		c.insight_id,
		c.chunk_kind,
		c.search_document <@> to_bm25query($2, 'insight_search_chunks_bm25_idx') AS bm25_distance,
		row_number() OVER (
			ORDER BY c.search_document <@> to_bm25query($2, 'insight_search_chunks_bm25_idx'), c.updated_at DESC
		) AS bm25_rank
	FROM insight_search_chunks c
	JOIN insights i
		ON i.id = c.insight_id
		AND i.tenant_key = c.tenant_key
	WHERE c.tenant_key = $1
		AND i.lifecycle_state = 'active'
		AND i.review_state = 'approved'
		AND (jsonb_array_length($5::jsonb) = 0 OR COALESCE(i.tags, '[]'::jsonb) @> $5::jsonb)
	ORDER BY c.search_document <@> to_bm25query($2, 'insight_search_chunks_bm25_idx'), c.updated_at DESC
	LIMIT $3
),
best_chunks AS (
	SELECT DISTINCT ON (insight_id)
		insight_id,
		chunk_kind,
		bm25_distance,
		bm25_rank
	FROM ranked_chunks
	WHERE bm25_distance < -0.000001
	ORDER BY insight_id, bm25_rank
),
vote_stats AS (
	SELECT
		v.insight_id,
		sum(CASE
			WHEN v.outcome = 'solved' THEN 1.0
			WHEN v.outcome = 'helped' THEN 0.5
			ELSE 0.0
		END) AS positive,
		sum(CASE
			WHEN v.outcome IN ('did_not_work', 'incorrect') THEN 1.0
			WHEN v.outcome = 'stale' THEN 0.5
			ELSE 0.0
		END) AS negative
	FROM votes v
	JOIN best_chunks b ON b.insight_id = v.insight_id
	WHERE v.tenant_key = $1
	GROUP BY v.insight_id
)
SELECT
	b.insight_id,
	(2.0 / (60.0 + b.bm25_rank)) *
		GREATEST(0.75, LEAST(1.35,
			0.75 + (
				COALESCE(v.positive, 0.0)::double precision /
				(COALESCE(v.positive, 0.0) + COALESCE(v.negative, 0.0) + 3.0)
			)
		)) AS score,
	b.bm25_rank,
	-b.bm25_distance AS bm25_score,
	COALESCE(v.positive, 0.0) AS positive,
	COALESCE(v.negative, 0.0) AS negative,
	b.chunk_kind
FROM best_chunks b
LEFT JOIN vote_stats v ON v.insight_id = b.insight_id
ORDER BY score DESC, b.bm25_rank ASC
LIMIT $4`
