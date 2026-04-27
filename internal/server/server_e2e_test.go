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
	svc := consensus.NewService(db.Client)

	handler, err := server.NewAPI(server.Config{Service: svc})
	require.NoError(t, err)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	client := consensusv1connect.NewInsightServiceClient(httpServer.Client(), httpServer.URL)

	created, err := client.Create(ctx, &consensusv1.InsightServiceCreateRequest{
		Title:   "PostHog source maps reject duplicate uploads",
		Problem: "A source map upload fails when the same commit is uploaded twice.",
		Answer:  "Source map uploads can fail when the same commit is uploaded twice.",
		Example: &consensusv1.InsightExample{
			Command:     "posthog sourcemaps upload",
			Description: "Duplicate commit upload path.",
		},
		Detail: "The upload path should handle duplicate commit attempts explicitly.",
		Action: "Check existing uploads before retrying the same commit.",
		Tags:   []string{"posthog", "source-maps", "build", "tool:turbo"},
		Links: []*consensusv1.InsightLink{{
			Title:       "PostHog source maps",
			Uri:         "https://posthog.com/docs/error-tracking/upload-source-maps",
			Description: "Source map upload behavior.",
		}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GetInsight().GetId())

	distractor, err := client.Create(ctx, &consensusv1.InsightServiceCreateRequest{
		Title:   "Duplicate commit cleanup for source map artifacts",
		Problem: "A build leaves duplicate source map artifacts after a commit is processed.",
		Answer:  "Remove stale generated artifacts before publishing build output.",
		Detail:  "This is unrelated to PostHog upload idempotency and does not mention the upload command.",
		Action:  "Clean the build directory and rerun the artifact packaging step.",
		Tags:    []string{"build", "source-maps"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, distractor.GetInsight().GetId())

	search, err := client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "posthog sourcemaps upload duplicate commit",
		Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.GetResults())
	require.Equal(t, created.GetInsight().GetId(), search.GetResults()[0].GetInsight().GetId())
	require.Contains(t, search.GetResults()[0].GetMatchedSignals(), "bm25")
	require.Contains(t, search.GetResults()[0].GetRankReason(), "BM25")

	search, err = client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "source maps duplicate commit",
		Tags:  []string{"posthog"},
		Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.GetResults())
	require.Equal(t, created.GetInsight().GetId(), search.GetResults()[0].GetInsight().GetId())
	require.Contains(t, search.GetResults()[0].GetMatchedSignals(), "tag")

	search, err = client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "source maps duplicate commit",
		Tags:  []string{"missing-tag"},
		Limit: 5,
	})
	require.NoError(t, err)
	require.Empty(t, search.GetResults())

	search, err = client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "source maps duplicate commit",
		Tags:  []string{"tool:turbo"},
		Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.GetResults())
	require.Equal(t, created.GetInsight().GetId(), search.GetResults()[0].GetInsight().GetId())
	require.Contains(t, search.GetResults()[0].GetMatchedSignals(), "tag")

	search, err = client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "source maps duplicate commit",
		Tags:  []string{"tool:vite"},
		Limit: 5,
	})
	require.NoError(t, err)
	require.Empty(t, search.GetResults())

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

	search, err = client.Search(ctx, &consensusv1.InsightServiceSearchRequest{
		Query: "skip duplicate uploads retry commit",
		Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, search.GetResults())
	require.Equal(t, created.GetInsight().GetId(), search.GetResults()[0].GetInsight().GetId())

	outcome, err := client.RecordOutcome(ctx, &consensusv1.InsightServiceRecordOutcomeRequest{
		InsightRef: created.GetInsight().GetId(),
		Outcome:    "solved",
		Rationale:  "The duplicate upload check avoided retrying the same commit.",
	})
	require.NoError(t, err)
	require.NotEmpty(t, outcome.GetOutcomeId())

	stats, err := svc.VoteStatsForInsights(ctx, []*consensusv1.Insight{created.GetInsight()})
	require.NoError(t, err)
	require.Equal(t, 1, stats[created.GetInsight().GetId()].Up)
	require.Equal(t, 1, stats[created.GetInsight().GetId()].Total)
	require.Equal(t, 1, stats[created.GetInsight().GetId()].Rank())
}
