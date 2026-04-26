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
	"github.com/loewenthal-corp/consensus/internal/postgres/graphedge"
	"github.com/loewenthal-corp/consensus/internal/postgres/knowledgeunit"
	"github.com/loewenthal-corp/consensus/internal/postgres/predicate"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const defaultTenantKey = "default"

type Service struct {
	db *postgres.Client
}

func NewService(db *postgres.Client) *Service {
	return &Service{db: db}
}

func (s *Service) Search(ctx context.Context, req *consensusv1.KnowledgeServiceSearchRequest) (*consensusv1.KnowledgeServiceSearchResponse, error) {
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
			knowledgeunit.Or(knowledgeTextPredicates(rawQuery)...),
		).
		Limit(limit).
		Order(postgres.Desc(knowledgeunit.FieldUpdatedAt))

	units, err := query.All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("search knowledge: %w", err))
	}

	results := make([]*consensusv1.KnowledgeSearchResult, 0, len(units))
	for _, unit := range units {
		results = append(results, &consensusv1.KnowledgeSearchResult{
			Unit:           toProtoKnowledgeUnit(unit),
			Score:          1,
			RankReason:     "matched text fields",
			MatchedSignals: []string{"text"},
		})
	}
	return &consensusv1.KnowledgeServiceSearchResponse{Results: results}, nil
}

func knowledgeTextPredicates(query string) []predicate.KnowledgeUnit {
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

	predicates := make([]predicate.KnowledgeUnit, 0, len(terms)*4)
	for _, term := range terms {
		predicates = append(predicates,
			knowledgeunit.TitleContainsFold(term),
			knowledgeunit.SummaryContainsFold(term),
			knowledgeunit.DetailContainsFold(term),
			knowledgeunit.ActionContainsFold(term),
		)
	}
	return predicates
}

func (s *Service) Get(ctx context.Context, req *consensusv1.KnowledgeServiceGetRequest) (*consensusv1.KnowledgeServiceGetResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid id: %w", err))
	}

	unit, err := s.db.KnowledgeUnit.Query().
		Where(knowledgeunit.TenantKey(defaultTenantKey), knowledgeunit.ID(id)).
		Only(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("knowledge unit not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get knowledge: %w", err))
	}
	return &consensusv1.KnowledgeServiceGetResponse{Unit: toProtoKnowledgeUnit(unit)}, nil
}

func (s *Service) Contribute(ctx context.Context, req *consensusv1.KnowledgeServiceContributeRequest) (*consensusv1.KnowledgeServiceContributeResponse, error) {
	unit, err := s.db.KnowledgeUnit.Create().
		SetTenantKey(defaultTenantKey).
		SetTitle(req.GetTitle()).
		SetSummary(req.GetSummary()).
		SetDetail(req.GetDetail()).
		SetAction(req.GetAction()).
		SetKind(defaultString(req.GetKind(), "finding")).
		SetLabels(req.GetLabels()).
		SetContext(req.GetContext()).
		SetEvidenceRefs(evidenceRefsToJSON(req.GetEvidenceRefs())).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create knowledge: %w", err))
	}

	return &consensusv1.KnowledgeServiceContributeResponse{
		Unit:          toProtoKnowledgeUnit(unit),
		PendingReview: false,
	}, nil
}

func (s *Service) Update(ctx context.Context, req *consensusv1.KnowledgeServiceUpdateRequest) (*consensusv1.KnowledgeServiceUpdateResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid id: %w", err))
	}

	update := s.db.KnowledgeUnit.UpdateOneID(id)
	if req.GetTitle() != "" {
		update.SetTitle(req.GetTitle())
	}
	if req.GetSummary() != "" {
		update.SetSummary(req.GetSummary())
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
	if req.GetLabels() != nil {
		update.SetLabels(req.GetLabels())
	}
	if req.GetContext() != nil {
		update.SetContext(req.GetContext())
	}
	if req.GetEvidenceRefs() != nil {
		update.SetEvidenceRefs(evidenceRefsToJSON(req.GetEvidenceRefs()))
	}

	unit, err := update.Save(ctx)
	if err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("knowledge unit not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update knowledge: %w", err))
	}
	return &consensusv1.KnowledgeServiceUpdateResponse{Unit: toProtoKnowledgeUnit(unit)}, nil
}

func (s *Service) Cast(ctx context.Context, req *consensusv1.VoteServiceCastRequest) (*consensusv1.VoteServiceCastResponse, error) {
	knowledgeUnitID, err := uuid.Parse(req.GetKnowledgeUnitId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid knowledge_unit_id: %w", err))
	}

	created, err := s.db.Vote.Create().
		SetTenantKey(defaultTenantKey).
		SetKnowledgeUnitID(knowledgeUnitID).
		SetOutcome(req.GetOutcome()).
		SetConfidence(req.GetConfidence()).
		SetRationale(req.GetRationale()).
		SetNillableIdempotencyKey(nilIfEmpty(req.GetIdempotencyKey())).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("cast vote: %w", err))
	}
	return &consensusv1.VoteServiceCastResponse{VoteId: created.ID.String()}, nil
}

func (s *Service) Retract(ctx context.Context, req *consensusv1.VoteServiceRetractRequest) (*consensusv1.VoteServiceRetractResponse, error) {
	id, err := uuid.Parse(req.GetVoteId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid vote_id: %w", err))
	}
	if err := s.db.Vote.DeleteOneID(id).Exec(ctx); err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("vote not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("retract vote: %w", err))
	}
	return &consensusv1.VoteServiceRetractResponse{}, nil
}

func (s *Service) Link(ctx context.Context, req *consensusv1.GraphServiceLinkRequest) (*consensusv1.GraphServiceLinkResponse, error) {
	fromID, err := uuid.Parse(req.GetFromId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid from_id: %w", err))
	}
	toID, err := uuid.Parse(req.GetToId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid to_id: %w", err))
	}

	edge, err := s.db.GraphEdge.Create().
		SetTenantKey(defaultTenantKey).
		SetFromID(fromID).
		SetToID(toID).
		SetRelationship(req.GetRelationship()).
		SetRationale(req.GetRationale()).
		Save(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("link graph nodes: %w", err))
	}
	return &consensusv1.GraphServiceLinkResponse{EdgeId: edge.ID.String()}, nil
}

func (s *Service) Unlink(ctx context.Context, req *consensusv1.GraphServiceUnlinkRequest) (*consensusv1.GraphServiceUnlinkResponse, error) {
	id, err := uuid.Parse(req.GetEdgeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid edge_id: %w", err))
	}
	if err := s.db.GraphEdge.DeleteOneID(id).Exec(ctx); err != nil {
		if postgres.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("graph edge not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("unlink graph nodes: %w", err))
	}
	return &consensusv1.GraphServiceUnlinkResponse{}, nil
}

func (s *Service) Neighbors(ctx context.Context, req *consensusv1.GraphServiceNeighborsRequest) (*consensusv1.GraphServiceNeighborsResponse, error) {
	nodeID, err := uuid.Parse(req.GetNodeId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid node_id: %w", err))
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 10
	}

	edges, err := s.db.GraphEdge.Query().
		Where(
			graphedge.TenantKey(defaultTenantKey),
			graphedge.Or(graphedge.FromID(nodeID), graphedge.ToID(nodeID)),
		).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("query graph neighbors: %w", err))
	}

	return &consensusv1.GraphServiceNeighborsResponse{
		Edges: toProtoGraphEdges(edges),
	}, nil
}

func (s *Service) ExplainPath(ctx context.Context, req *consensusv1.GraphServiceExplainPathRequest) (*consensusv1.GraphServiceExplainPathResponse, error) {
	fromID, err := uuid.Parse(req.GetFromId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid from_id: %w", err))
	}
	toID, err := uuid.Parse(req.GetToId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid to_id: %w", err))
	}

	edges, err := s.db.GraphEdge.Query().
		Where(
			graphedge.TenantKey(defaultTenantKey),
			graphedge.FromID(fromID),
			graphedge.ToID(toID),
		).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("explain graph path: %w", err))
	}

	explanation := "No direct path found."
	if len(edges) > 0 {
		explanation = "Direct graph edge found."
	}
	return &consensusv1.GraphServiceExplainPathResponse{
		Explanation: explanation,
		Edges:       toProtoGraphEdges(edges),
	}, nil
}

func toProtoKnowledgeUnit(unit *postgres.KnowledgeUnit) *consensusv1.KnowledgeUnit {
	if unit == nil {
		return nil
	}
	out := &consensusv1.KnowledgeUnit{
		Id:             unit.ID.String(),
		Title:          unit.Title,
		Summary:        unit.Summary,
		Detail:         unit.Detail,
		Action:         unit.Action,
		Kind:           unit.Kind,
		Labels:         unit.Labels,
		Context:        unit.Context,
		EvidenceRefs:   evidenceRefsFromJSON(unit.EvidenceRefs),
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

func evidenceRefsToJSON(refs []*consensusv1.EvidenceRef) []map[string]string {
	out := make([]map[string]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]string{
			"kind":    ref.GetKind(),
			"title":   ref.GetTitle(),
			"uri":     ref.GetUri(),
			"excerpt": ref.GetExcerpt(),
		})
	}
	return out
}

func evidenceRefsFromJSON(refs []map[string]string) []*consensusv1.EvidenceRef {
	out := make([]*consensusv1.EvidenceRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, &consensusv1.EvidenceRef{
			Kind:    ref["kind"],
			Title:   ref["title"],
			Uri:     ref["uri"],
			Excerpt: ref["excerpt"],
		})
	}
	return out
}

func toProtoGraphEdges(edges []*postgres.GraphEdge) []*consensusv1.GraphEdge {
	out := make([]*consensusv1.GraphEdge, 0, len(edges))
	for _, edge := range edges {
		out = append(out, &consensusv1.GraphEdge{
			Id:           edge.ID.String(),
			FromId:       edge.FromID.String(),
			ToId:         edge.ToID.String(),
			Relationship: edge.Relationship,
			Rationale:    edge.Rationale,
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
