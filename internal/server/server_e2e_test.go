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

func TestServer_ConnectInsightFlow(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(ctx, t)

	handler, err := server.NewAPI(server.Config{Service: consensus.NewService(db.Client)})
	require.NoError(t, err)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client := consensusv1connect.NewInsightServiceClient(httpServer.Client(), httpServer.URL)

	created, err := client.Create(ctx, &consensusv1.InsightServiceCreateRequest{
		Title:   "PostHog source maps reject duplicate uploads",
		Problem: "A source map upload fails when the same commit is uploaded twice.",
		Answer:  "Source map uploads can fail when the same commit is uploaded twice.",
		Example: &consensusv1.InsightExample{
			Kind:        "command",
			Command:     "posthog sourcemaps upload",
			Description: "Duplicate commit upload path.",
		},
		Detail:  "The upload path should handle duplicate commit attempts explicitly.",
		Action:  "Check existing uploads before retrying the same commit.",
		Kind:    "pitfall",
		Tags:    []string{"posthog", "source-maps", "build"},
		Context: map[string]string{"tool": "turbo"},
		Links: []*consensusv1.InsightLink{{
			Kind:        "docs",
			Title:       "PostHog source maps",
			Uri:         "https://posthog.com/docs/error-tracking/upload-source-maps",
			Description: "Source map upload behavior.",
		}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetInsight().GetId())

	search, err := client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "source maps duplicate commit",
		Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, search.GetResults(), 1)
	require.Equal(t, created.GetInsight().GetId(), search.GetResults()[0].GetInsight().GetId())

	got, err := client.Get(ctx, &consensusv1.InsightServiceGetRequest{Ref: created.GetInsight().GetId()})
	require.NoError(t, err)
	require.Equal(t, created.GetInsight().GetTitle(), got.GetInsight().GetTitle())
	require.Equal(t, created.GetInsight().GetAnswer(), got.GetInsight().GetAnswer())
	require.Len(t, got.GetInsight().GetLinks(), 1)

	updated, err := client.Update(ctx, &consensusv1.InsightServiceUpdateRequest{
		Id:     created.GetInsight().GetId(),
		Action: "Skip duplicate uploads before retrying the same commit.",
	})
	require.NoError(t, err)
	require.Equal(t, "Skip duplicate uploads before retrying the same commit.", updated.GetInsight().GetAction())

	outcome, err := client.RecordOutcome(ctx, &consensusv1.InsightServiceRecordOutcomeRequest{
		InsightRef: created.GetInsight().GetId(),
		Outcome:    "solved",
		Rationale:  "The duplicate upload check avoided retrying the same commit.",
	})
	require.NoError(t, err)
	require.NotEmpty(t, outcome.GetOutcomeId())
}
