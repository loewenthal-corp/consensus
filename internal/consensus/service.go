package consensus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	consensusv1 "github.com/loewenthal-corp/consensus/internal/gen/consensus/v1"
	"github.com/loewenthal-corp/consensus/internal/postgres"
	"github.com/loewenthal-corp/consensus/internal/postgres/insight"
	"github.com/loewenthal-corp/consensus/internal/search"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const defaultTenantKey = "default"

var validOutcomes = map[string]struct{}{
	"solved":         {},
	"helped":         {},
	"did_not_work":   {},
	"stale":          {},
	"incorrect":      {},
	"not_applicable": {},
}

type Service struct {
	db       *postgres.Client
	searcher search.Searcher
}

func NewService(db *postgres.Client) *Service {
	svc := &Service{db: db}
	if db != nil {
		if sqlDB := db.SQLDB(); sqlDB != nil {
			svc.searcher = search.NewPostgresSearcher(sqlDB)
		}
	}
	return svc
}

func (s *Service) ListRecentInsights(ctx context.Context, limit int) ([]*consensusv1.Insight, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 25
	}

	insights, err := s.db.Insight.Query().
		Where(insight.TenantKey(defaultTenantKey)).
		Order(postgres.Desc(insight.FieldUpdatedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent insights: %w", err)
	}

	out := make([]*consensusv1.Insight, 0, len(insights))
	for _, item := range insights {
		out = append(out, toProtoInsight(item))
	}
	return out, nil
}

func (s *Service) Search(ctx context.Context, req *consensusv1.InsightServiceSearchRequest) (*consensusv1.InsightServiceSearchResponse, error) {
	rawQuery := strings.TrimSpace(req.GetQuery())
	if rawQuery == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}

	if s.db == nil || s.searcher == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("search backend is not configured"))
	}

	searchResults, err := s.searcher.Search(ctx, search.Request{
		TenantKey: defaultTenantKey,
		Query:     rawQuery,
		Limit:     int(req.GetLimit()),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("search insights: %w", err))
	}
	if len(searchResults) == 0 {
		return &consensusv1.InsightServiceSearchResponse{}, nil
	}

	ids := make([]uuid.UUID, 0, len(searchResults))
	for _, result := range searchResults {
		ids = append(ids, result.InsightID)
	}

	insights, err := s.db.Insight.Query().
		Where(insight.TenantKey(defaultTenantKey), insight.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load search insights: %w", err))
	}
	byID := make(map[uuid.UUID]*postgres.Insight, len(insights))
	for _, item := range insights {
		byID[item.ID] = item
	}

	results := make([]*consensusv1.InsightSearchResult, 0, len(searchResults))
	for _, result := range searchResults {
		item := byID[result.InsightID]
		if item == nil {
			continue
		}
		results = append(results, &consensusv1.InsightSearchResult{
			Insight:        toProtoInsight(item),
			Score:          result.Score,
			RankReason:     result.RankReason,
			MatchedSignals: result.MatchedSignals,
		})
	}
	return &consensusv1.InsightServiceSearchResponse{Results: results}, nil
}

func (s *Service) Get(ctx context.Context, req *consensusv1.InsightServiceGetRequest) (*consensusv1.InsightServiceGetResponse, error) {
	id, err := parseLocalInsightRef(req.GetRef())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	item, err := s.db.Insight.Query().
		Where(insight.TenantKey(defaultTenantKey), insight.ID(id)).
		Only(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("insight not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get insight: %w", err))
	}
	return &consensusv1.InsightServiceGetResponse{Insight: toProtoInsight(item)}, nil
}

func (s *Service) Create(ctx context.Context, req *consensusv1.InsightServiceCreateRequest) (*consensusv1.InsightServiceCreateResponse, error) {
	item, err := s.db.Insight.Create().
		SetTenantKey(defaultTenantKey).
		SetTitle(req.GetTitle()).
		SetProblem(req.GetProblem()).
		SetAnswer(req.GetAnswer()).
		SetExample(insightExampleToJSON(req.GetExample())).
		SetDetail(req.GetDetail()).
		SetAction(req.GetAction()).
		SetKind(defaultString(req.GetKind(), "insight")).
		SetTags(req.GetTags()).
		SetContext(req.GetContext()).
		SetLinks(insightLinksToJSON(req.GetLinks())).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create insight: %w", err))
	}
	if err := s.indexInsight(ctx, item); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("index insight search chunks: %w", err))
	}

	return &consensusv1.InsightServiceCreateResponse{
		Insight:       toProtoInsight(item),
		PendingReview: false,
	}, nil
}

func (s *Service) Update(ctx context.Context, req *consensusv1.InsightServiceUpdateRequest) (*consensusv1.InsightServiceUpdateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid id: %w", err))
	}

	update := s.db.Insight.UpdateOneID(id)
	if req.GetTitle() != "" {
		update.SetTitle(req.GetTitle())
	}
	if req.GetProblem() != "" {
		update.SetProblem(req.GetProblem())
	}
	if req.GetAnswer() != "" {
		update.SetAnswer(req.GetAnswer())
	}
	if req.GetExample() != nil {
		update.SetExample(insightExampleToJSON(req.GetExample()))
	}
	if req.GetDetail() != "" {
		update.SetDetail(req.GetDetail())
	}
	if req.GetAction() != "" {
		update.SetAction(req.GetAction())
	}
	if req.GetKind() != "" {
		update.SetKind(req.GetKind())
	}
	if req.GetTags() != nil {
		update.SetTags(req.GetTags())
	}
	if req.GetContext() != nil {
		update.SetContext(req.GetContext())
	}
	if req.GetLinks() != nil {
		update.SetLinks(insightLinksToJSON(req.GetLinks()))
	}

	item, err := update.Save(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("insight not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update insight: %w", err))
	}
	if err := s.indexInsight(ctx, item); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("index insight search chunks: %w", err))
	}
	return &consensusv1.InsightServiceUpdateResponse{Insight: toProtoInsight(item)}, nil
}

func (s *Service) RecordOutcome(ctx context.Context, req *consensusv1.InsightServiceRecordOutcomeRequest) (*consensusv1.InsightServiceRecordOutcomeResponse, error) {
	outcome := strings.TrimSpace(req.GetOutcome())
	if _, ok := validOutcomes[outcome]; !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unsupported outcome %q", outcome))
	}

	insightID, err := parseLocalInsightRef(req.GetInsightRef())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	created, err := s.db.Vote.Create().
		SetTenantKey(defaultTenantKey).
		SetInsightID(insightID).
		SetOutcome(outcome).
		SetConfidence(req.GetConfidence()).
		SetRationale(req.GetRationale()).
		SetNillableIdempotencyKey(nilIfEmpty(req.GetIdempotencyKey())).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("record outcome: %w", err))
	}
	return &consensusv1.InsightServiceRecordOutcomeResponse{OutcomeId: created.ID.String()}, nil
}

func (s *Service) indexInsight(ctx context.Context, item *postgres.Insight) error {
	if s == nil || s.searcher == nil {
		return nil
	}
	return s.searcher.IndexInsight(ctx, item)
}

func parseLocalInsightRef(ref string) (uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return uuid.Nil, errors.New("insight ref is required")
	}

	for _, prefix := range []string{"consensus://insight/"} {
		if strings.HasPrefix(ref, prefix) {
			ref = strings.TrimPrefix(ref, prefix)
			break
		}
	}

	id, err := uuid.Parse(ref)
	if err != nil {
		return uuid.Nil, fmt.Errorf("unsupported insight ref %q: use a local UUID; federated refs require an upstream dispatcher", ref)
	}
	return id, nil
}

func toProtoInsight(item *postgres.Insight) *consensusv1.Insight {
	if item == nil {
		return nil
	}
	out := &consensusv1.Insight{
		Id:             item.ID.String(),
		Title:          item.Title,
		Problem:        item.Problem,
		Answer:         item.Answer,
		Example:        insightExampleFromJSON(item.Example),
		Detail:         item.Detail,
		Action:         item.Action,
		Kind:           item.Kind,
		Tags:           item.Tags,
		Context:        item.Context,
		Links:          insightLinksFromJSON(item.Links),
		ReviewState:    item.ReviewState,
		LifecycleState: item.LifecycleState,
		CreatedAt:      timestamppb.New(item.CreatedAt),
		UpdatedAt:      timestamppb.New(item.UpdatedAt),
	}
	if item.LastConfirmedAt != nil {
		out.LastConfirmedAt = timestamppb.New(*item.LastConfirmedAt)
	}
	return out
}

func insightExampleToJSON(example *consensusv1.InsightExample) map[string]string {
	if example == nil {
		return map[string]string{}
	}
	return map[string]string{
		"kind":        example.GetKind(),
		"language":    example.GetLanguage(),
		"content":     example.GetContent(),
		"command":     example.GetCommand(),
		"description": example.GetDescription(),
	}
}

func insightExampleFromJSON(example map[string]string) *consensusv1.InsightExample {
	if len(example) == 0 {
		return nil
	}
	out := &consensusv1.InsightExample{
		Kind:        example["kind"],
		Language:    example["language"],
		Content:     example["content"],
		Command:     example["command"],
		Description: example["description"],
	}
	if out.GetKind() == "" && out.GetLanguage() == "" && out.GetContent() == "" && out.GetCommand() == "" && out.GetDescription() == "" {
		return nil
	}
	return out
}

func insightLinksToJSON(links []*consensusv1.InsightLink) []map[string]string {
	out := make([]map[string]string, 0, len(links))
	for _, link := range links {
		out = append(out, map[string]string{
			"kind":        link.GetKind(),
			"uri":         link.GetUri(),
			"title":       link.GetTitle(),
			"description": link.GetDescription(),
			"relation":    link.GetRelation(),
			"excerpt":     link.GetExcerpt(),
		})
	}
	return out
}

func insightLinksFromJSON(links []map[string]string) []*consensusv1.InsightLink {
	out := make([]*consensusv1.InsightLink, 0, len(links))
	for _, link := range links {
		out = append(out, &consensusv1.InsightLink{
			Kind:        link["kind"],
			Uri:         link["uri"],
			Title:       link["title"],
			Description: link["description"],
			Relation:    link["relation"],
			Excerpt:     link["excerpt"],
		})
	}
	return out
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
