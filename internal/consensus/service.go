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
	"github.com/loewenthal-corp/consensus/internal/postgres/knowledgeunit"
	"github.com/loewenthal-corp/consensus/internal/postgres/predicate"
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
	db *postgres.Client
}

func NewService(db *postgres.Client) *Service {
	return &Service{db: db}
}

func (s *Service) ListRecentInsights(ctx context.Context, limit int) ([]*consensusv1.Insight, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 25
	}

	units, err := s.db.KnowledgeUnit.Query().
		Where(knowledgeunit.TenantKey(defaultTenantKey)).
		Order(postgres.Desc(knowledgeunit.FieldUpdatedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list recent insights: %w", err)
	}

	out := make([]*consensusv1.Insight, 0, len(units))
	for _, unit := range units {
		out = append(out, toProtoInsight(unit))
	}
	return out, nil
}

func (s *Service) Search(ctx context.Context, req *consensusv1.InsightServiceSearchRequest) (*consensusv1.InsightServiceSearchResponse, error) {
	rawQuery := strings.TrimSpace(req.GetQuery())
	if rawQuery == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 10
	}

	query := s.db.KnowledgeUnit.Query().
		Where(
			knowledgeunit.TenantKey(defaultTenantKey),
			knowledgeunit.LifecycleState("active"),
			knowledgeunit.Or(insightTextPredicates(rawQuery)...),
		).
		Limit(limit).
		Order(postgres.Desc(knowledgeunit.FieldUpdatedAt))

	units, err := query.All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("search insights: %w", err))
	}

	results := make([]*consensusv1.InsightSearchResult, 0, len(units))
	for _, unit := range units {
		results = append(results, &consensusv1.InsightSearchResult{
			Insight:        toProtoInsight(unit),
			Score:          1,
			RankReason:     "matched text fields",
			MatchedSignals: []string{"text"},
		})
	}
	return &consensusv1.InsightServiceSearchResponse{Results: results}, nil
}

func insightTextPredicates(query string) []predicate.KnowledgeUnit {
	seen := make(map[string]struct{})
	terms := make([]string, 0, 1+len(strings.Fields(query)))

	addTerm := func(term string) {
		term = strings.TrimSpace(term)
		if term == "" {
			return
		}
		key := strings.ToLower(term)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		terms = append(terms, term)
	}

	addTerm(query)
	for _, term := range strings.Fields(query) {
		addTerm(term)
	}

	predicates := make([]predicate.KnowledgeUnit, 0, len(terms)*5)
	for _, term := range terms {
		predicates = append(predicates,
			knowledgeunit.TitleContainsFold(term),
			knowledgeunit.ProblemContainsFold(term),
			knowledgeunit.SummaryContainsFold(term),
			knowledgeunit.DetailContainsFold(term),
			knowledgeunit.ActionContainsFold(term),
		)
	}
	return predicates
}

func (s *Service) Get(ctx context.Context, req *consensusv1.InsightServiceGetRequest) (*consensusv1.InsightServiceGetResponse, error) {
	id, err := parseLocalInsightRef(req.GetRef())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	unit, err := s.db.KnowledgeUnit.Query().
		Where(knowledgeunit.TenantKey(defaultTenantKey), knowledgeunit.ID(id)).
		Only(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("insight not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get insight: %w", err))
	}
	return &consensusv1.InsightServiceGetResponse{Insight: toProtoInsight(unit)}, nil
}

func (s *Service) Create(ctx context.Context, req *consensusv1.InsightServiceCreateRequest) (*consensusv1.InsightServiceCreateResponse, error) {
	unit, err := s.db.KnowledgeUnit.Create().
		SetTenantKey(defaultTenantKey).
		SetTitle(req.GetTitle()).
		SetProblem(req.GetProblem()).
		SetSummary(req.GetAnswer()).
		SetExample(insightExampleToJSON(req.GetExample())).
		SetDetail(req.GetDetail()).
		SetAction(req.GetAction()).
		SetKind(defaultString(req.GetKind(), "insight")).
		SetLabels(req.GetTags()).
		SetContext(req.GetContext()).
		SetEvidenceRefs(insightLinksToJSON(req.GetLinks())).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create insight: %w", err))
	}

	return &consensusv1.InsightServiceCreateResponse{
		Insight:       toProtoInsight(unit),
		PendingReview: false,
	}, nil
}

func (s *Service) Update(ctx context.Context, req *consensusv1.InsightServiceUpdateRequest) (*consensusv1.InsightServiceUpdateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid id: %w", err))
	}

	update := s.db.KnowledgeUnit.UpdateOneID(id)
	if req.GetTitle() != "" {
		update.SetTitle(req.GetTitle())
	}
	if req.GetProblem() != "" {
		update.SetProblem(req.GetProblem())
	}
	if req.GetAnswer() != "" {
		update.SetSummary(req.GetAnswer())
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
		update.SetLabels(req.GetTags())
	}
	if req.GetContext() != nil {
		update.SetContext(req.GetContext())
	}
	if req.GetLinks() != nil {
		update.SetEvidenceRefs(insightLinksToJSON(req.GetLinks()))
	}

	unit, err := update.Save(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("insight not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update insight: %w", err))
	}
	return &consensusv1.InsightServiceUpdateResponse{Insight: toProtoInsight(unit)}, nil
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
		SetKnowledgeUnitID(insightID).
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

func toProtoInsight(unit *postgres.KnowledgeUnit) *consensusv1.Insight {
	if unit == nil {
		return nil
	}
	out := &consensusv1.Insight{
		Id:             unit.ID.String(),
		Title:          unit.Title,
		Problem:        unit.Problem,
		Answer:         unit.Summary,
		Example:        insightExampleFromJSON(unit.Example),
		Detail:         unit.Detail,
		Action:         unit.Action,
		Kind:           unit.Kind,
		Tags:           unit.Labels,
		Context:        unit.Context,
		Links:          insightLinksFromJSON(unit.EvidenceRefs),
		ReviewState:    unit.ReviewState,
		LifecycleState: unit.LifecycleState,
		CreatedAt:      timestamppb.New(unit.CreatedAt),
		UpdatedAt:      timestamppb.New(unit.UpdatedAt),
	}
	if unit.LastConfirmedAt != nil {
		out.LastConfirmedAt = timestamppb.New(*unit.LastConfirmedAt)
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
