# Contributing

Consensus should stay aggressively simple. The project is a small MCP component
that helps agents reuse prior answers; it should not grow into a full agent
workflow engine.

## Design Principles

- Keep the MCP surface tiny. A new tool must be more valuable than the context it
  costs every agent that sees it.
- Put agent-facing guidance in Protobuf comments, MCP tool descriptions, and
  schemas. Do not require a skill for the normal happy path.
- Prefer one-shot retrieval. The server should return useful results, links, and
  ranking rationale; the agent decides what to do next.
- Store insights, not notes. A good insight has a concrete problem, answer,
  action, optional example, and evidence links.
- Keep administrative operations out of default MCP tools. Use Connect API and
  `/admin` for edits, review, moderation, federation settings, and operations.
- Make search deterministic and explainable before making it clever. BM25,
  exact/context matches, links, and outcomes should remain visible ranking
  signals even if vector search or reranking is added later.
- Keep the default deployment boring: one Go binary and Postgres.

## Project Layout

- `cmd/consensus`: server entrypoint and runtime configuration.
- `proto/consensus/v1`: public Protobuf contracts. Comments here become
  agent-facing documentation.
- `internal/server`: HTTP, Connect, MCP registration, admin handlers, and
  request logging.
- `internal/consensus`: application service implementation.
- `internal/postgres`: Ent schema and generated persistence code.
- `internal/search`: search chunks, Postgres BM25 search, and search interfaces.
- `containers`: local Docker Compose setup.
- `docs`: deeper architecture, search, and benchmark notes.

## Development Setup

Required tools:

- Go `1.26.2` or newer compatible toolchain.
- `task`.
- Docker for local Postgres and Testcontainers-backed tests.

Common commands:

```sh
task generate          # regenerate protobuf and Ent code
task format            # go fmt and buf format
task lint              # go vet and buf lint
task test              # unit tests
task test::e2e         # Testcontainers-backed end-to-end tests
task build             # build /tmp/consensus
task do                # generate, format, lint, test, build
task containers::up    # local Postgres + Consensus stack
task containers::down  # stop local stack
```

## Working On MCP Tools

The default MCP surface is allowlisted in `internal/server/server.go`. Adding a
method to the Protobuf service does not automatically mean it should become an
agent tool.

Before adding or exposing a tool, answer these questions in the PR:

- Can the agent already do this with `search`, `get`, `create`, or
  `record_outcome`?
- Is this useful often enough to justify the tool schema and description tokens?
- Does it belong in default agent context, or should it stay in Connect API or
  `/admin`?
- Can a cheap model understand when to use it from the tool description alone?

When changing Protobuf comments or fields, run:

```sh
task generate::proto
```

## Working On Insights

Insights should be short enough to read directly into context and specific
enough to retrieve later. Avoid storing whole conversations, long logs, or vague
lessons like "check the config."

Prefer examples that help future retrieval:

- exact error text
- failing command
- relevant config snippet
- stack trace excerpt
- package, framework, service, or version combination
- source issue, PR, ticket, trace, or test proof link

## Working On Search

Search should optimize for high value per context token. The server should return
a small result set with enough rank reason and matched signals for an agent to
decide whether to use the result.

The default path should stay Postgres-native. External search services, required
LLM rerankers, or extra infrastructure need a strong reason and should be
optional.

## Testing Expectations

For normal code changes, run:

```sh
task test
```

For Protobuf, Ent schema, search schema, MCP registration, or server behavior,
also run the relevant generation or e2e checks:

```sh
task generate
task test::e2e
```

Use focused tests for the changed behavior. Broaden coverage when a change
touches shared contracts, ranking behavior, or MCP/API compatibility.
