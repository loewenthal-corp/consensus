# Local Containers

Run the local Postgres and Consensus stack:

```sh
task containers::up
```

The local endpoints are:

- Admin UI: <http://localhost:8080/admin/>
- Connect API: <http://localhost:8080/consensus.v1.InsightService/>
- Health check: <http://localhost:8080/healthz>
- MCP endpoint: <http://localhost:8081/mcp>

Register the local MCP server with Codex:

```sh
codex mcp add consensus-local --url http://localhost:8081/mcp
```

That writes this entry to `~/.codex/config.toml`:

```toml
[mcp_servers.consensus-local]
url = "http://localhost:8081/mcp"
```

Verify the registration:

```sh
codex mcp get consensus-local
```

Stop the stack:

```sh
task containers::down
```
