package search

import (
	"context"

	"github.com/google/uuid"
	"github.com/loewenthal-corp/consensus/internal/postgres"
)

type Request struct {
	TenantKey string
	Query     string
	Tags      []string
	Limit     int
}

type Result struct {
	InsightID      uuid.UUID
	Score          float64
	RankReason     string
	MatchedSignals []string
}

type Searcher interface {
	Search(context.Context, Request) ([]Result, error)
	IndexInsight(context.Context, *postgres.Insight) error
}
