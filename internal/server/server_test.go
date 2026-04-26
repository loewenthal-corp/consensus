package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	"github.com/loewenthal-corp/consensus/internal/server"
)

func TestServer_HealthAndAdmin(t *testing.T) {
	handler, err := server.New(server.Config{Service: consensus.NewService(nil)})
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
	require.Contains(t, rec.Body.String(), "Consensus Admin")
}

func TestServer_MCPListsGeneratedTools(t *testing.T) {
	handler, err := server.New(server.Config{Service: consensus.NewService(nil)})
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
	require.Contains(t, string(respBody), "market-based context engineering")

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
	require.Contains(t, text, "consensus_v1_KnowledgeService_Search")
	require.Contains(t, text, "Search returns ranked knowledge units")
	require.Contains(t, text, "rediscovering an answer")
}
