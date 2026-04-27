package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	consensusv1 "github.com/loewenthal-corp/consensus/internal/gen/consensus/v1"
	"github.com/loewenthal-corp/consensus/internal/gen/consensus/v1/consensusv1connect"
	"github.com/loewenthal-corp/consensus/internal/server"
)

func TestServer_HealthAndAdmin(t *testing.T) {
	handler, err := server.NewAPI(server.Config{Service: consensus.NewService(nil)})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"ok"`)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	adminHTML := rec.Body.String()
	require.Contains(t, adminHTML, "Consensus Admin")
	require.Contains(t, adminHTML, "MCP Search")
	require.Contains(t, adminHTML, `name="query"`)
	require.Contains(t, adminHTML, `name="tags"`)
	require.NotContains(t, adminHTML, `name="context"`)
	require.Contains(t, adminHTML, `name="include_upstreams"`)
	require.Contains(t, adminHTML, "Recent Insights")

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/?query=posthog&tags=build%20tool%3Aturbo&limit=5&include_upstreams=true", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	adminHTML = rec.Body.String()
	require.Contains(t, adminHTML, "search backend is not configured")
	require.Contains(t, adminHTML, `value="posthog"`)
	require.Contains(t, adminHTML, `value="build tool:turbo"`)
	require.Contains(t, adminHTML, "checked")

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_MCPListsAllowlistedTools(t *testing.T) {
	handler, err := server.NewMCP(server.Config{Service: consensus.NewService(nil)})
	require.NoError(t, err)

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0.0.0"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Mcp-Session-Id"))

	respBody, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	require.Contains(t, string(respBody), "Consensus")
	require.Contains(t, string(respBody), "Search when prior agent work may help")
	require.Contains(t, string(respBody), `"tools":{}`)
	require.NotContains(t, string(respBody), `"logging":{}`)
	require.NotContains(t, string(respBody), `"listChanged":true`)

	sessionID := rec.Header().Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)

	body = strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	req = httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Contains(t, []int{http.StatusAccepted, http.StatusOK}, rec.Code)

	body = strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	req = httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	respBody, err = io.ReadAll(rec.Body)
	require.NoError(t, err)
	text := string(respBody)
	require.Contains(t, text, `"name":"search"`)
	require.Contains(t, text, `"name":"get"`)
	require.Contains(t, text, `"name":"create"`)
	require.Contains(t, text, `"name":"record_outcome"`)
	require.Contains(t, text, "Find prior insights")
	require.NotContains(t, text, `"context"`)
	require.NotContains(t, text, `"kind"`)
	require.NotContains(t, text, "one-shot retrieval tool")
	require.NotContains(t, text, "consensus_v1_InsightService_Search")
	require.NotContains(t, text, "consensus_v1_InsightService_Update")
	require.NotContains(t, text, "VoteService")
	require.NotContains(t, text, "GraphService")
}

func TestServer_ConnectDebugLogsInsightExchange(t *testing.T) {
	var logs bytes.Buffer
	restoreDefaultLogger(t, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	handler, err := server.NewAPI(server.Config{Service: consensus.NewService(nil)})
	require.NoError(t, err)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client := consensusv1connect.NewInsightServiceClient(httpServer.Client(), httpServer.URL)
	_, err = client.Search(context.Background(), &consensusv1.InsightServiceSearchRequest{
		Query: "posthog sourcemaps upload duplicate commit",
		Limit: 5,
	})
	require.Error(t, err)

	text := logs.String()
	require.Contains(t, text, `"msg":"insight exchange"`)
	require.Contains(t, text, `"transport":"connect"`)
	require.Contains(t, text, `"procedure":"/consensus.v1.InsightService/Search"`)
	require.Contains(t, text, `"outcome":"error"`)
	require.Contains(t, text, `"code":"failed_precondition"`)
	require.Contains(t, text, `"query":"posthog sourcemaps upload duplicate commit"`)
	require.Contains(t, text, `"limit":5`)
}

func TestServer_MCPDebugLogsInsightExchange(t *testing.T) {
	var logs bytes.Buffer
	restoreDefaultLogger(t, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	handler, err := server.NewMCP(server.Config{Service: consensus.NewService(nil)})
	require.NoError(t, err)

	sessionID := initializeMCP(t, handler)
	body := strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"posthog sourcemaps upload duplicate commit","limit":5}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	text := logs.String()
	require.Contains(t, text, `"msg":"insight exchange"`)
	require.Contains(t, text, `"transport":"mcp"`)
	require.Contains(t, text, `"method":"consensus.v1.InsightService.Search"`)
	require.Contains(t, text, `"tool":"search"`)
	require.Contains(t, text, `"outcome":"error"`)
	require.Contains(t, text, `"query":"posthog sourcemaps upload duplicate commit"`)
	require.Contains(t, text, `"limit":5`)
}

func TestServer_MCPDoesNotServeAPIOrAdmin(t *testing.T) {
	handler, err := server.NewMCP(server.Config{Service: consensus.NewService(nil)})
	require.NoError(t, err)

	for _, path := range []string{"/admin/", "/healthz", "/consensus.v1.InsightService/Search"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
}

func initializeMCP(t *testing.T, handler http.Handler) string {
	t.Helper()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0.0.0"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	sessionID := rec.Header().Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)

	body = strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	req = httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Contains(t, []int{http.StatusAccepted, http.StatusOK}, rec.Code)

	return sessionID
}

func restoreDefaultLogger(t *testing.T, logger *slog.Logger) {
	t.Helper()

	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
}
