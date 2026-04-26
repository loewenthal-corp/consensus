//go:build e2e

package server_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	consensusv1 "github.com/loewenthal-corp/consensus/internal/gen/consensus/v1"
	"github.com/loewenthal-corp/consensus/internal/gen/consensus/v1/consensusv1connect"
	"github.com/loewenthal-corp/consensus/internal/postgres/postgrestest"
	"github.com/loewenthal-corp/consensus/internal/server"
)

func TestServer_ConnectKnowledgeFlow(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(ctx, t)

	handler, err := server.New(server.Config{Service: consensus.NewService(db.Client)})
	require.NoError(t, err)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client := consensusv1connect.NewKnowledgeServiceClient(httpServer.Client(), httpServer.URL)

	created, err := client.Contribute(ctx, &consensusv1.KnowledgeServiceContributeRequest{
		Title:   "PostHog source maps reject duplicate uploads",
		Summary: "Source map uploads can fail when the same commit is uploaded twice.",
		Detail:  "The upload path should handle duplicate commit attempts explicitly.",
		Action:  "Check existing uploads before retrying the same commit.",
		Kind:    "pitfall",
		Labels:  []string{"posthog", "source-maps", "build"},
		Context: map[string]string{"tool": "turbo"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetUnit().GetId())

	search, err := client.Search(ctx, &consensusv1.KnowledgeServiceSearchRequest{
		Query: "source maps duplicate commit",
		Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, search.GetResults(), 1)
	require.Equal(t, created.GetUnit().GetId(), search.GetResults()[0].GetUnit().GetId())

	got, err := client.Get(ctx, &consensusv1.KnowledgeServiceGetRequest{Id: created.GetUnit().GetId()})
	require.NoError(t, err)
	require.Equal(t, created.GetUnit().GetTitle(), got.GetUnit().GetTitle())
}
