# Search Architecture Proposal

Status: implementation started

Information about extension maturity and version support is current as of
April 26, 2026.

This document proposes the search architecture for Consensus. The goal is to
provide high-quality insight retrieval inside Postgres, with no external search
service in the default deployment, while leaving room for model and ranking
tuning as real outcome data accumulates.

The proposal takes inspiration from Tobi Lutke's QMD pipeline: lexical search,
semantic vector search, reciprocal rank fusion, and optional reranking over a
small candidate set. Consensus should adopt that retrieval shape, but implement
the storage and ranking layer with Postgres-native extensions and Go services.

## Goals

- Retrieve the right insight for a concrete problem, error, command, stack
  trace, code snippet, repo area, tool, or service.
- Combine exact keyword matching with semantic matching.
- Prefer insights that actually solved or helped previous work, without letting
  popularity overwhelm relevance.
- Keep the system operationally simple: one Go service and one Postgres
  database by default.
- Keep ranking explainable enough that agents and humans can understand why a
  result was returned.
- Support progressive rollout: useful built-in Postgres search first, then true
  BM25, then vector search, then optional reranking.
- Avoid introducing Elasticsearch, OpenSearch, Pinecone, Qdrant, or another
  required service unless Postgres-native search clearly fails at production
  scale.

## Non-Goals

- Consensus is not a generic vector database wrapper.
- Consensus should not rely on vector search alone. Exact identifiers, error
  messages, commands, package names, and API names are often decisive.
- Consensus should not store whole conversations as searchable documents. The
  durable unit remains an answer-shaped insight.
- Consensus should not make LLM reranking mandatory for correctness. Reranking
  should improve quality, not be the only thing preventing bad retrieval.
- Consensus should not hide ranking logic in a black box. SQL-visible signals
  and score traces are part of the product.

## Current Implementation

`InsightService.Search` now uses Postgres-backed search chunks and
`pg_textsearch` BM25 ranking when the service runs against a Postgres image with
the extension preloaded. Insight create and update paths synchronously refresh a
single `main` search chunk built from title, problem, answer, action, detail,
example, tags, context, and links. Search runs raw SQL over
`insight_search_chunks`, collapses chunk results back to insights, returns BM25
matched signals, and applies a bounded vote quality multiplier.

Local Docker Compose and Testcontainers use `timescale/timescaledb-ha:pg17`
with `shared_preload_libraries=timescaledb,pg_textsearch`.

Before this implementation, `InsightService.Search` performed
case-insensitive substring matching across the main insight fields and ordered
results by `updated_at`. The remaining limitations are:

- No semantic search over embeddings.
- No true multi-chunk splitting beyond the initial one-chunk-per-insight shape.
- No vector retrieval or embedding generation.
- No exact fingerprint or graph candidate lists in RRF yet.
- No detailed score trace beyond compact rank reasons and matched signals.

The existing data model already has useful primitives:

- `insights` store title, problem, answer, detail, action, examples, tags,
  context, links, review state, lifecycle state, and freshness.
- `votes` store outcomes such as `solved`, `helped`, `did_not_work`, `stale`,
  and `incorrect`.
- `problem_fingerprints` store exact-ish matching fields such as error hashes,
  commands, toolchains, services, repo path patterns, environments, and
  dependency versions.
- `graph_edges` can later provide related, same-root-cause, supersedes,
  requires, and contradicts signals.

Search should build on these tables rather than replacing them.

## Recommended Stack

The recommended production path is:

| Layer | Choice | Reason |
| --- | --- | --- |
| Lexical search | `pg_textsearch` | True BM25 ranking in Postgres, Postgres license, supports Postgres 17 and 18, production-ready as of April 2026. |
| Semantic search | `pgvector` | Mature Postgres vector extension with HNSW and IVFFlat indexes. |
| Large-scale vector option | `pgvectorscale` | Optional later upgrade for StreamingDiskANN if HNSW memory or latency becomes limiting. |
| Fallback lexical search | Postgres `tsvector` + GIN + `websearch_to_tsquery` | Works without third-party search extensions and is much better than substring matching. |
| Fuzzy matching | Postgres `pg_trgm` | Useful for typo tolerance and partial identifier matching. |
| Fusion | Reciprocal Rank Fusion in SQL | Combines BM25, vector, exact, context, graph, and outcome rankings without normalizing incompatible score scales. |
| Optional reranking | Cross-encoder or small LLM over top chunks | Improves quality for ambiguous queries while bounding latency and token cost. |

`pg_textsearch` should be the preferred BM25 option for the default
self-hosted path because it has a permissive Postgres license. ParadeDB
`pg_search` remains worth evaluating for Elastic-like features such as richer
faceting, but its AGPL license and moving API make it a less conservative
default.

## Baseline Fallback

Before requiring `pg_textsearch`, Consensus can ship a pure-Postgres fallback:

- Add a stored generated `tsvector` or expression GIN index over the searchable
  insight text.
- Use `websearch_to_tsquery('english', query)` for forgiving query syntax.
- Use `ts_rank_cd` for lexical ranking.
- Add `pg_trgm` indexes for fuzzy title/problem and exact-ish identifier search.

This baseline is not true BM25. It does not use global corpus statistics the
way modern search engines do. It is still a large improvement over
`ContainsFold` predicates and gives a low-risk migration path.

## Searchable Document Shape

Consensus should retrieve at the insight level but index at the chunk level.
Whole insights are compact, but detail fields, examples, stack traces, and links
can grow enough that chunking improves vector quality and reranking cost.

Proposed table:

```sql
CREATE TABLE insight_search_chunks (
    id uuid PRIMARY KEY,
    tenant_key text NOT NULL,
    insight_id uuid NOT NULL,
    chunk_kind text NOT NULL,
    chunk_ordinal int NOT NULL,
    chunk_text text NOT NULL,
    embedding vector(1536),
    embedding_model text,
    content_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
```

Suggested `chunk_kind` values:

- `title_problem`: title plus problem/symptom text.
- `answer_action`: concise answer plus recommended action.
- `detail`: longer explanation and caveats.
- `example`: command, code, config, exact error, log, or stack trace.
- `links`: useful excerpts from evidence links when safe and compact.

The first implementation can create one chunk per insight from title, problem,
answer, action, and detail. More precise chunking can follow once longer
examples and evidence excerpts become common.

## Indexing Plan

### BM25

When `pg_textsearch` is available, create BM25 indexes over weighted text. The
exact index expression needs testing, but the intent is:

- Higher weight for title, problem, and exact error/example text.
- Medium weight for answer and action.
- Lower weight for long detail and evidence excerpts.

If a single expression index is used, construct an immutable search document
from normalized fields:

```sql
CREATE INDEX insight_chunks_bm25_idx
ON insight_search_chunks
USING bm25 ((chunk_kind || ' ' || chunk_text))
WITH (text_config = 'english');
```

If separate indexes perform better, maintain one BM25 index per important text
shape and fuse them as independent lexical lists.

### Full-Text Fallback

Fallback index:

```sql
ALTER TABLE insight_search_chunks
ADD COLUMN search_vector tsvector
GENERATED ALWAYS AS (
    to_tsvector('english', coalesce(chunk_kind, '') || ' ' || coalesce(chunk_text, ''))
) STORED;

CREATE INDEX insight_chunks_search_vector_idx
ON insight_search_chunks
USING gin (search_vector);
```

### Vector

Use cosine distance by default:

```sql
CREATE INDEX insight_chunks_embedding_hnsw_idx
ON insight_search_chunks
USING hnsw (embedding vector_cosine_ops);
```

Start with `pgvector` HNSW. Consider `pgvectorscale` and `diskann` later if
the corpus reaches a size where HNSW memory, build time, or latency becomes a
material issue.

### Tenant, Lifecycle, and Context

Search should always be tenant scoped. Lifecycle and review filters should be
cheap:

```sql
CREATE INDEX insights_search_scope_idx
ON insights (tenant_key, lifecycle_state, review_state);

CREATE INDEX insight_chunks_scope_idx
ON insight_search_chunks (tenant_key, insight_id);
```

Tags and structured context are currently JSON. For early versions, JSONB
filters are acceptable. If tag/context filtering becomes central to ranking,
normalize them into side tables so they can participate in fast joins and
statistics.

## Query Pipeline

The default search pipeline should be:

1. Normalize the request.
2. Build lexical candidates.
3. Build semantic candidates if an embedder is configured and chunk embeddings
   are available.
4. Build exact/context candidates from tags, context, problem fingerprints, and
   exact strings.
5. Build outcome/value candidates from aggregated vote statistics.
6. Fuse candidates with reciprocal rank fusion.
7. Apply lifecycle, review, tenant, permission, and deduplication rules.
8. Optionally rerank the top 30-50 chunks.
9. Collapse chunk results to insight results.
10. Return score, rank reason, and matched signals.

The service should over-fetch internally and return a small final result set:

| Candidate list | Initial limit |
| --- | ---: |
| BM25 lexical | 30 |
| Vector semantic | 30 |
| Exact/context/fingerprint | 20 |
| Outcome/value prior | 100 |
| Rerank candidates | 30-50 |
| Default returned results | 5-10 |
| Maximum returned results | 50 |

## Reciprocal Rank Fusion

RRF should be the first fusion strategy because it is robust when combining
signals with incompatible score scales. BM25 scores, vector similarities,
recency values, and vote counts should not be naively added together.

Basic formula:

```text
rrf(document) = sum(weight_i / (k + rank_i(document)))
```

Use `k = 60` as the starting point. Initial weights:

| Signal | Weight |
| --- | ---: |
| Original query BM25 | 2.0 |
| Original query vector | 2.0 |
| Expanded lexical query | 1.0 |
| Expanded vector query | 1.0 |
| Exact fingerprint match | 3.0 |
| Tag/context match | 1.0 |
| Vote/value prior | 0.5 |
| Freshness prior | 0.25 |

Exact fingerprint matches can deserve more weight than BM25 or vector matches
because a known error hash, failing command, toolchain, or service can be more
diagnostic than semantic similarity.

## Vote and Value Ranking

Outcome votes should affect ranking, but only after relevance has found
plausible candidates. A broadly popular insight should not outrank a precise
match for an exact error.

Recommended approach:

- Use votes as a weak independent RRF list to keep high-utility insights visible.
- Also apply a bounded quality multiplier after RRF.
- Cap the multiplier so vote count cannot dominate relevance.
- Penalize stale, incorrect, and did-not-work outcomes more strongly when they
  are recent.

Example quality score:

```text
positive = solved + 0.5 * helped
negative = did_not_work + incorrect + 0.5 * stale
confidence = positive / (positive + negative + 3)
quality_multiplier = clamp(0.75 + confidence, 0.75, 1.35)
```

This keeps unproven insights usable, rewards proven insights, and prevents one
old high-vote artifact from permanently owning broad queries.

## Freshness and Lifecycle

Freshness should depend on the kind of insight:

- Pitfalls, commands, version-specific bugs, API behavior, and deployment
  runbooks decay faster.
- General design guidance and stable policy decay slower.
- `last_confirmed_at` should matter more than `updated_at` when available.
- `stale`, `superseded`, `archived`, and `deleted` lifecycle states should be
  filtered or heavily penalized by default.

Freshness should be a bounded multiplier, not the primary ranking signal.

## Query Expansion

QMD uses typed query expansion: lexical variants route to full-text search,
semantic variants route to vector search, and HyDE-style hypothetical documents
route to vector search.

Consensus can support the same idea later, but it should not be required for
the first production search implementation. The first version should search the
user's original problem statement well.

Future expansion shape:

```text
lex: exact error words, package names, commands, flags, product names
vec: natural-language restatement of the situation
hyde: short hypothetical insight that would answer the query
```

The expansion output should be structured and bounded. It should not be free
form prompt text passed directly into SQL.

## Reranking

Reranking should be optional and bounded:

- Only rerank the top 30-50 fused candidates.
- Rerank chunks, not whole insight bodies.
- Cache rerank results by query, model, and chunk hash.
- Blend reranker score with the RRF position score.
- Protect the top few retrieval results from being completely overturned by a
  noisy reranker.

Suggested blend:

```text
if rrf_rank <= 3:  rrf_weight = 0.75
if rrf_rank <= 10: rrf_weight = 0.60
else:              rrf_weight = 0.40

final = rrf_weight * (1 / rrf_rank) + (1 - rrf_weight) * rerank_score
```

Reranking is a quality enhancer. The non-reranked path must still be good
enough to serve production traffic.

## Go Service Design

Keep Ent for normal CRUD and use raw SQL for search. Search needs CTEs, window
functions, BM25 operators, vector distance operators, rank fusion, and score
tracing. Those queries will be clearer and easier to tune in SQL than through
Ent predicates.

Proposed package shape:

```text
internal/search/
    searcher.go        request/response types and Searcher interface
    postgres.go        Postgres implementation with raw SQL
    embedder.go        Embedder interface
    explain.go         rank reason and matched signal construction
    chunks.go          chunk text builder and content hash helpers
```

The service layer should depend on an interface:

```go
type Searcher interface {
    Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
}

type Embedder interface {
    Embed(ctx context.Context, text string) (Embedding, error)
}

type Embedding struct {
    Model string
    Dims  int
    Data  []float32
}
```

Embedding generation can be provided by OpenAI, a local model, or another
provider. The search architecture should not care as long as embeddings are
written to Postgres in a consistent dimension and model namespace.

## Background Jobs

Search quality depends on derived data. These should run through jobs rather
than blocking all write paths:

- Create or refresh search chunks after insight create/update.
- Generate embeddings for missing or stale chunks.
- Recompute content hashes.
- Aggregate vote statistics per insight.
- Detect near-duplicates.
- Rebuild or refresh indexes after bulk imports.

For early versions, create chunks synchronously and queue embeddings
asynchronously. Search should gracefully skip vector retrieval when embeddings
are unavailable.

## Rank Reasons and Matched Signals

Every returned result should include a compact explanation. Examples:

- `matched exact error hash and ranked highly in BM25`
- `matched source maps tags and semantic similarity`
- `ranked highly by vector search and has solved outcomes`
- `matched command text; penalized by recent did_not_work outcome`

`matched_signals` should use stable machine-readable values:

- `bm25`
- `vector`
- `exact_error_hash`
- `command`
- `tag`
- `context`
- `outcome`
- `freshness`
- `graph`
- `rerank`

This is useful for agents, admin review, and ranking evaluation.

## Evaluation Plan

Search should be evaluated with fixtures before it is trusted:

- Exact error query should retrieve the known matching insight at rank 1.
- Natural-language paraphrase should retrieve the same insight through vector
  search.
- Common broad query should not let popularity swamp relevance.
- Stale or incorrect insights should fall below active confirmed insights.
- Exact command and package names should beat semantically related but different
  tools.
- Queries with no good answer should return few or no results rather than a
  confident-looking weak match.

Track at least:

- Recall at 5.
- Mean reciprocal rank.
- Search latency p50/p95.
- Candidate counts by source.
- Empty-result rate.
- Outcome rate after search.
- Did-not-work rate after search.

## Rollout Plan

### Phase 1: Indexed Built-In Search

- Add search chunks.
- Add Postgres FTS fallback with `tsvector` and GIN.
- Add `pg_trgm` for typo and substring support.
- Add vote aggregation and simple bounded quality multipliers.
- Replace substring search with SQL-ranked lexical search.

### Phase 2: BM25

- Add `pg_textsearch` to local and test Postgres images.
- Add BM25 indexes.
- Implement BM25 candidate retrieval and RRF.
- Keep the FTS fallback path for deployments without the extension.

### Phase 3: Vector Search

- Add `pgvector` to local and test Postgres images.
- Add embeddings to `insight_search_chunks`.
- Add an embedder interface and background embedding job.
- Add vector candidates and RRF fusion.

### Phase 4: Explainability and Tuning

- Add detailed rank traces in admin/debug paths.
- Add search evaluation fixtures.
- Tune signal weights, candidate limits, and freshness multipliers.

### Phase 5: Optional Reranking

- Add optional reranker interface.
- Rerank only top chunks.
- Cache rerank scores.
- Evaluate whether the quality gain justifies latency and model cost.

## Operational Notes

- Local Docker and testcontainers should use an image with the needed
  extensions installed.
- Production startup should verify required extensions and expose a clear
  health or diagnostics error when configured search capabilities are missing.
- Search should degrade by capability:
  - BM25 unavailable: use built-in FTS.
  - embeddings unavailable: skip vector candidates.
  - reranker unavailable: return fused candidates.
- Approximate vector indexes need empirical tuning. Start with HNSW defaults,
  then tune `hnsw.ef_search`, `m`, and `ef_construction` only after measuring
  recall and latency.
- Reindexing and embedding model changes should be explicit operational events.
  Store `embedding_model` with each chunk so mixed-model data can be detected.

## Open Questions

- Which embedding model should be the default for local and hosted deployments?
- Should chunk embeddings use only answer-shaped fields, or include evidence
  excerpts and examples by default?
- Should tags/context remain JSONB, or move to normalized tables before search
  tuning begins?
- What is the minimum result quality threshold for returning no result?
- Should upstream/federated results participate in the same RRF query, or be
  fused after local search?
- How aggressive should stale and superseded penalties be for old but still
  heavily solved insights?

## References

- QMD: <https://github.com/tobi/qmd>
- pg_textsearch: <https://github.com/timescale/pg_textsearch>
- pgvector: <https://github.com/pgvector/pgvector>
- pgvectorscale: <https://github.com/timescale/pgvectorscale>
- ParadeDB: <https://www.paradedb.com/>
- Postgres full-text search: <https://www.postgresql.org/docs/current/textsearch.html>
- Postgres text search functions: <https://www.postgresql.org/docs/current/functions-textsearch.html>
- Postgres `pg_trgm`: <https://www.postgresql.org/docs/current/pgtrgm.html>
