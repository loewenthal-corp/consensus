package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/gen"
	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime/gosdk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/loewenthal-corp/consensus/internal/buildinfo"
	consensus "github.com/loewenthal-corp/consensus/internal/consensus"
	consensusv1 "github.com/loewenthal-corp/consensus/internal/gen/consensus/v1"
	"github.com/loewenthal-corp/consensus/internal/gen/consensus/v1/consensusv1connect"
	insighttags "github.com/loewenthal-corp/consensus/internal/tags"
)

type Config struct {
	Service *consensus.Service
}

func NewAPI(cfg Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, fmt.Errorf("service is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /admin/", handleAdmin(cfg.Service))

	if err := registerConnectHandlers(mux, cfg.Service); err != nil {
		return nil, err
	}

	return requestMiddleware(mux), nil
}

func NewMCP(cfg Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, fmt.Errorf("service is required")
	}

	mcpHandler, err := newMCPHandler(cfg.Service)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	return requestMiddleware(mux), nil
}

func registerConnectHandlers(mux *http.ServeMux, svc *consensus.Service) error {
	otelInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTrustRemote(),
		otelconnect.WithPropagateResponseHeader(),
	)
	if err != nil {
		return fmt.Errorf("create otel interceptor: %w", err)
	}

	path, handler := consensusv1connect.NewInsightServiceHandler(svc,
		connect.WithInterceptors(newInsightExchangeLoggingInterceptor(nil), otelInterceptor),
	)
	mux.Handle(path, handler)
	return nil
}

func newMCPHandler(svc *consensus.Service) (http.Handler, error) {
	raw := mcp.NewServer(&mcp.Implementation{
		Name:    "consensus",
		Title:   "Consensus",
		Version: buildinfo.Version,
	}, &mcp.ServerOptions{
		Instructions: mcpInstructions,
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
	})
	mcpServer := gosdk.Wrap(raw)
	handler := serviceDispatcher(svc)

	for _, serviceName := range []string{
		"consensus.v1.InsightService",
	} {
		sd, err := findServiceDescriptor(serviceName)
		if err != nil {
			return nil, err
		}
		if registered := registerMCPTools(mcpServer, sd, handler); registered == 0 {
			return nil, fmt.Errorf("no MCP tools registered for %s", serviceName)
		}
	}

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return raw
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
	}), nil
}

const mcpInstructions = `Search when prior agent work may help. Get promising matches. After applying an insight, record solved/helped/did_not_work/stale/incorrect. Create compact evidence-backed insights only for reusable discoveries.`

var mcpToolNames = map[protoreflect.FullName]string{
	"consensus.v1.InsightService.Search":        "search",
	"consensus.v1.InsightService.Get":           "get",
	"consensus.v1.InsightService.Create":        "create",
	"consensus.v1.InsightService.RecordOutcome": "record_outcome",
}

var mcpToolFallbackComments = map[protoreflect.FullName]string{
	"consensus.v1.InsightService.Search":        `Find prior insights for a concrete problem, error, command, stack trace, snippet, or repo/tool detail.`,
	"consensus.v1.InsightService.Get":           `Fetch one insight by ID or consensus URI.`,
	"consensus.v1.InsightService.Create":        `Submit a compact evidence-backed insight after solving something reusable; avoid transcripts or notes.`,
	"consensus.v1.InsightService.RecordOutcome": `Record solved/helped/did_not_work/stale/incorrect after applying an insight. did_not_work means tried and failed; ignore irrelevant search results.`,
}

var mcpMethodAllowlist = map[protoreflect.FullName]struct{}{
	"consensus.v1.InsightService.Search":        {},
	"consensus.v1.InsightService.Get":           {},
	"consensus.v1.InsightService.Create":        {},
	"consensus.v1.InsightService.RecordOutcome": {},
}

func mcpToolComment(method protoreflect.MethodDescriptor) string {
	if location := method.ParentFile().SourceLocations().ByDescriptor(method); strings.TrimSpace(location.LeadingComments) != "" {
		return location.LeadingComments
	}
	return mcpToolFallbackComments[method.FullName()]
}

func mcpToolName(method protoreflect.MethodDescriptor) string {
	if name := mcpToolNames[method.FullName()]; name != "" {
		return name
	}
	return strings.ReplaceAll(string(method.FullName()), ".", "_")
}

func registerMCPTools(s runtime.MCPServer, sd protoreflect.ServiceDescriptor, handler gen.Handler) int {
	registered := 0
	for i := range sd.Methods().Len() {
		method := sd.Methods().Get(i)
		if method.IsStreamingClient() || method.IsStreamingServer() {
			continue
		}
		if _, ok := mcpMethodAllowlist[method.FullName()]; !ok {
			continue
		}

		tool, _ := gen.ToolForMethod(method, mcpToolComment(method))
		tool.Name = mcpToolName(method)
		md := method
		toolName := tool.Name
		s.AddTool(tool, func(ctx context.Context, request *runtime.CallToolRequest) (*runtime.CallToolResult, error) {
			ctx, finishExchange := beginMCPInsightExchange(ctx, md, toolName)
			var concreteReq proto.Message
			var concreteResp proto.Message
			var exchangeErr error
			defer func() {
				finishExchange(concreteReq, concreteResp, request.Arguments, exchangeErr)
			}()

			marshaled, err := json.Marshal(request.Arguments)
			if err != nil {
				exchangeErr = err
				return nil, err
			}
			concreteReq = gen.DynamicNewMessage(md.Input())
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(marshaled, concreteReq); err != nil {
				exchangeErr = err
				return nil, err
			}

			concreteResp, err = handler(ctx, md, concreteReq)
			if err != nil {
				exchangeErr = err
				return runtime.HandleError(err)
			}

			marshaled, err = (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(concreteResp)
			if err != nil {
				exchangeErr = err
				return nil, err
			}
			return runtime.NewToolResultText(string(marshaled)), nil
		})
		registered++
	}
	return registered
}

func serviceDispatcher(svc *consensus.Service) gen.Handler {
	return func(ctx context.Context, method protoreflect.MethodDescriptor, req proto.Message) (proto.Message, error) {
		switch string(method.FullName()) {
		case "consensus.v1.InsightService.Search":
			var concrete consensusv1.InsightServiceSearchRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Search(ctx, &concrete)
		case "consensus.v1.InsightService.Get":
			var concrete consensusv1.InsightServiceGetRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Get(ctx, &concrete)
		case "consensus.v1.InsightService.Create":
			var concrete consensusv1.InsightServiceCreateRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Create(ctx, &concrete)
		case "consensus.v1.InsightService.RecordOutcome":
			var concrete consensusv1.InsightServiceRecordOutcomeRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.RecordOutcome(ctx, &concrete)
		default:
			return nil, fmt.Errorf("unhandled MCP method %s", method.FullName())
		}
	}
}

func remarshal(src proto.Message, dst proto.Message) error {
	body, err := protojson.Marshal(src)
	if err != nil {
		return fmt.Errorf("marshal dynamic proto: %w", err)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, dst); err != nil {
		return fmt.Errorf("unmarshal concrete proto: %w", err)
	}
	return nil
}

func findServiceDescriptor(fullName string) (protoreflect.ServiceDescriptor, error) {
	var found protoreflect.ServiceDescriptor
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			if string(sd.FullName()) == fullName {
				found = sd
				return false
			}
		}
		return true
	})
	if found == nil {
		return nil, fmt.Errorf("service %q not found", fullName)
	}
	return found, nil
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "consensus",
	})
}

func handleAdmin(svc *consensus.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		pageSize := parseAdminInt(values.Get("page_size"), 25, 1, 100)
		recent, err := svc.ListRecentInsightsPage(r.Context(), parseAdminInt(values.Get("page"), 1, 1, 100000), pageSize)
		if recent == nil {
			recent = &consensus.InsightListPage{Page: 1, PageSize: pageSize, TotalPages: 1}
		}

		data := adminPageData{
			Recent:         recent,
			Search:         adminSearchFromRequest(r),
			ClearSearchURL: adminClearSearchURL(r),
		}
		if err != nil {
			data.Error = err.Error()
		} else {
			stats, err := svc.VoteStatsForInsights(r.Context(), recent.Insights)
			if err != nil {
				data.Error = err.Error()
			} else {
				data.RecentItems = adminItemsForInsights(recent.Insights, stats)
			}
		}
		if recent.Page > 1 {
			data.PreviousPageURL = adminPageURL(r, recent.Page-1)
		}
		if recent.Total > 0 && recent.Page < recent.TotalPages {
			data.NextPageURL = adminPageURL(r, recent.Page+1)
		}

		if strings.TrimSpace(data.Search.Query) != "" {
			data.Search.Searched = true
			resp, err := svc.Search(r.Context(), &consensusv1.InsightServiceSearchRequest{
				Query:            data.Search.Query,
				Tags:             parseAdminSearchTags(data.Search.Tags),
				Limit:            int32(data.Search.Limit),
				IncludeUpstreams: data.Search.IncludeUpstreams,
			})
			if err != nil {
				data.Search.Error = err.Error()
			} else {
				data.Search.Results = resp.GetResults()
				stats, err := svc.VoteStatsForInsights(r.Context(), adminInsightsFromResults(resp.GetResults()))
				if err != nil {
					data.Search.Error = err.Error()
				} else {
					data.SearchItems = adminItemsForSearchResults(resp.GetResults(), stats)
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := adminTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type adminPageData struct {
	Recent          *consensus.InsightListPage
	RecentItems     []adminInsightItem
	Search          adminSearchData
	SearchItems     []adminInsightItem
	PreviousPageURL string
	NextPageURL     string
	ClearSearchURL  string
	Error           string
}

type adminInsightItem struct {
	Insight        *consensusv1.Insight
	Stats          consensus.InsightVoteStats
	Score          float64
	RankReason     string
	MatchedSignals []string
	HasScore       bool
}

type adminSearchData struct {
	Query            string
	Tags             string
	Limit            int
	IncludeUpstreams bool
	Searched         bool
	Results          []*consensusv1.InsightSearchResult
	Error            string
}

func adminItemsForInsights(insights []*consensusv1.Insight, stats map[string]consensus.InsightVoteStats) []adminInsightItem {
	items := make([]adminInsightItem, 0, len(insights))
	for _, insight := range insights {
		if insight == nil {
			continue
		}
		items = append(items, adminInsightItem{
			Insight: insight,
			Stats:   stats[insight.GetId()],
		})
	}
	return items
}

func adminItemsForSearchResults(results []*consensusv1.InsightSearchResult, stats map[string]consensus.InsightVoteStats) []adminInsightItem {
	items := make([]adminInsightItem, 0, len(results))
	for _, result := range results {
		insight := result.GetInsight()
		if insight == nil {
			continue
		}
		items = append(items, adminInsightItem{
			Insight:        insight,
			Stats:          stats[insight.GetId()],
			Score:          result.GetScore(),
			RankReason:     result.GetRankReason(),
			MatchedSignals: result.GetMatchedSignals(),
			HasScore:       true,
		})
	}
	return items
}

func adminInsightsFromResults(results []*consensusv1.InsightSearchResult) []*consensusv1.Insight {
	insights := make([]*consensusv1.Insight, 0, len(results))
	for _, result := range results {
		if insight := result.GetInsight(); insight != nil {
			insights = append(insights, insight)
		}
	}
	return insights
}

func adminSearchFromRequest(r *http.Request) adminSearchData {
	values := r.URL.Query()
	return adminSearchData{
		Query:            strings.TrimSpace(values.Get("query")),
		Tags:             values.Get("tags"),
		Limit:            parseAdminInt(values.Get("limit"), 10, 0, 50),
		IncludeUpstreams: parseAdminBool(values.Get("include_upstreams")),
	}
}

func parseAdminInt(raw string, fallback, min, max int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func parseAdminBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func parseAdminSearchTags(raw string) []string {
	return insighttags.Parse(raw)
}

func adminPageURL(r *http.Request, page int) string {
	values := r.URL.Query()
	values.Set("page", strconv.Itoa(page))
	if values.Get("page_size") == "" {
		values.Set("page_size", "25")
	}
	return r.URL.Path + "?" + values.Encode()
}

func adminClearSearchURL(r *http.Request) string {
	values := r.URL.Query()
	for _, key := range []string{"query", "tags", "limit", "include_upstreams", "page"} {
		values.Del(key)
	}
	encoded := values.Encode()
	if encoded == "" {
		return "/admin/"
	}
	return "/admin/?" + encoded
}

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"join": strings.Join,
	"insightLabel": func(count int) string {
		if count == 1 {
			return "insight"
		}
		return "insights"
	},
	"formatTime": func(ts interface{ AsTime() time.Time }) string {
		if ts == nil {
			return ""
		}
		return ts.AsTime().Format("2006-01-02 15:04:05 MST")
	},
	"signedInt": func(value int) string {
		if value > 0 {
			return "+" + strconv.Itoa(value)
		}
		return strconv.Itoa(value)
	},
}).Parse(`{{define "insightCard"}}
<details class="insight-card">
  <summary class="insight-summary">
    <div class="vote-box">
      <div class="rank">{{signedInt .Stats.Rank}}</div>
      <div class="rank-label">rank</div>
      <div class="vote-row">
        <span>Up {{.Stats.Up}}</span>
        <span>Down {{.Stats.Down}}</span>
        {{if .Stats.Stale}}<span>Stale {{.Stats.Stale}}</span>{{end}}
      </div>
    </div>
    <div class="insight-main">
      <div class="title">{{.Insight.Title}}</div>
      <div class="summary-line">
        <span class="state">{{.Insight.ReviewState}} / {{.Insight.LifecycleState}}</span>
        <span>{{formatTime .Insight.UpdatedAt}}</span>
      </div>
    </div>
    <div class="search-rank">
      {{if .HasScore}}
      <div class="score">{{printf "%.6f" .Score}}</div>
      <div class="meta">search score</div>
      {{else}}
      <div class="score">{{.Stats.Total}}</div>
      <div class="meta">votes</div>
      {{end}}
    </div>
  </summary>
  <div class="detail">
    <div class="detail-grid">
      <div class="detail-section">
        <div class="detail-label">Answer</div>
        <div class="detail-text">{{.Insight.Answer}}</div>
      </div>
      {{if .Insight.Problem}}
      <div class="detail-section">
        <div class="detail-label">Problem</div>
        <div class="detail-text">{{.Insight.Problem}}</div>
      </div>
      {{end}}
      {{if .Insight.Action}}
      <div class="detail-section">
        <div class="detail-label">Action</div>
        <div class="detail-text">{{.Insight.Action}}</div>
      </div>
      {{end}}
      {{if .Insight.Detail}}
      <div class="detail-section">
        <div class="detail-label">Detail</div>
        <div class="detail-text">{{.Insight.Detail}}</div>
      </div>
      {{end}}
      {{with .Insight.Example}}
      <div class="detail-section">
        <div class="detail-label">Example</div>
        <div class="detail-text">{{if .Language}}{{.Language}}{{end}}{{if .Command}}
{{.Command}}{{end}}{{if .Description}}
{{.Description}}{{end}}{{if .Content}}
{{.Content}}{{end}}</div>
      </div>
      {{end}}
      <div class="detail-section">
        <div class="detail-label">Votes</div>
        <div class="detail-text">Up {{.Stats.Up}}, Down {{.Stats.Down}}, Stale {{.Stats.Stale}}, Other {{.Stats.Other}}, Total {{.Stats.Total}}</div>
      </div>
      {{if .HasScore}}
      <div class="detail-section">
        <div class="detail-label">Ranking</div>
        <div class="detail-text">{{.RankReason}}</div>
        <div class="signals">
          {{range .MatchedSignals}}<span class="signal">{{.}}</span>{{end}}
        </div>
      </div>
      {{end}}
      {{if .Insight.Tags}}
      <div class="detail-section">
        <div class="detail-label">Tags</div>
        <div class="detail-text">{{join .Insight.Tags ", "}}</div>
      </div>
      {{end}}
      <div class="detail-section">
        <div class="detail-label">Identity</div>
        <div class="detail-text">{{.Insight.Id}}
Created {{formatTime .Insight.CreatedAt}}</div>
      </div>
      {{if .Insight.Links}}
      <div class="detail-section detail-wide">
        <div class="detail-label">Links</div>
        <ul class="detail-list">
          {{range .Insight.Links}}
          <li class="detail-text">{{if .Title}}<span class="link-title">{{.Title}}</span>{{end}}{{if .Uri}}{{if .Title}} / {{end}}{{.Uri}}{{end}}{{if .Description}}
{{.Description}}{{end}}{{if .Excerpt}}
{{.Excerpt}}{{end}}</li>
          {{end}}
        </ul>
      </div>
      {{end}}
    </div>
  </div>
</details>
{{end}}
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Consensus Admin</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: #f7f7f4;
      color: #1f2528;
    }
    body {
      margin: 0;
      background: #f7f7f4;
      color: #1f2528;
    }
    main {
      max-width: 1180px;
      margin: 0 auto;
      padding: 32px 24px;
    }
    header {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 16px;
      border-bottom: 1px solid #d8ddd7;
      padding-bottom: 18px;
      margin-bottom: 20px;
    }
    h1 {
      font-size: 28px;
      line-height: 1.1;
      margin: 0;
      font-weight: 680;
    }
    .count {
      color: #576166;
      font-size: 14px;
      white-space: nowrap;
    }
    section {
      margin-top: 22px;
    }
    h2 {
      font-size: 17px;
      line-height: 1.2;
      margin: 0;
      font-weight: 680;
    }
    .section-heading {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 10px;
    }
    .error {
      border: 1px solid #c95252;
      background: #fff0f0;
      color: #8d1f1f;
      padding: 10px 12px;
      margin-bottom: 16px;
      border-radius: 6px;
      font-size: 14px;
    }
    .search-panel {
      border: 1px solid #d8ddd7;
      background: white;
      padding: 16px;
    }
    .search-grid {
      display: grid;
      grid-template-columns: minmax(220px, 2fr) minmax(150px, 1fr) minmax(170px, 1fr) 92px;
      gap: 12px;
      align-items: end;
    }
    .field {
      display: flex;
      flex-direction: column;
      gap: 6px;
      min-width: 0;
    }
    label, .field-label {
      color: #576166;
      font-size: 12px;
      font-weight: 650;
    }
    input[type="search"], input[type="text"], input[type="number"] {
      width: 100%;
      box-sizing: border-box;
      border: 1px solid #cfd6cf;
      background: #fbfcfa;
      color: #1f2528;
      border-radius: 6px;
      font: inherit;
      font-size: 14px;
      padding: 9px 10px;
      min-height: 38px;
    }
    input:focus {
      outline: 2px solid #8fb7a3;
      outline-offset: 1px;
      border-color: #6d9a84;
    }
    .search-options {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-top: 12px;
    }
    .check-row {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      color: #3d474b;
      font-size: 14px;
      font-weight: 520;
    }
    .actions {
      display: inline-flex;
      gap: 8px;
      align-items: center;
    }
    button, .button-link {
      border: 1px solid #283c32;
      background: #283c32;
      color: white;
      border-radius: 6px;
      cursor: pointer;
      font: inherit;
      font-size: 14px;
      font-weight: 650;
      min-height: 38px;
      padding: 8px 13px;
      text-decoration: none;
      display: inline-flex;
      align-items: center;
      justify-content: center;
    }
    .button-link.secondary {
      background: transparent;
      color: #283c32;
    }
    .insight-list {
      display: flex;
      flex-direction: column;
      gap: 10px;
    }
    .insight-card {
      border: 1px solid #d8ddd7;
      background: white;
      border-radius: 6px;
      overflow: hidden;
    }
    .insight-card[open] {
      border-color: #b8c5bc;
    }
    .insight-summary {
      cursor: pointer;
      display: grid;
      grid-template-columns: 96px minmax(0, 1fr) minmax(130px, auto);
      gap: 14px;
      align-items: center;
      list-style: none;
      padding: 14px 16px;
    }
    .insight-summary::-webkit-details-marker {
      display: none;
    }
    .vote-box {
      border-right: 1px solid #e5e8e2;
      padding-right: 12px;
      text-align: center;
    }
    .rank {
      color: #263f32;
      font-size: 22px;
      font-weight: 760;
      line-height: 1;
    }
    .rank-label {
      color: #687479;
      font-size: 11px;
      font-weight: 650;
      margin-top: 4px;
      text-transform: uppercase;
    }
    .vote-row {
      color: #4f5a5f;
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      justify-content: center;
      margin-top: 8px;
      font-size: 12px;
      line-height: 1.2;
    }
    .insight-main {
      min-width: 0;
    }
    .summary-line {
      color: #687479;
      display: flex;
      flex-wrap: wrap;
      gap: 7px;
      align-items: center;
      font-size: 13px;
      margin-top: 6px;
    }
    .badge {
      border: 1px solid #cfd6cf;
      background: #f5f7f3;
      border-radius: 999px;
      color: #35443b;
      display: inline-flex;
      font-size: 12px;
      line-height: 1;
      padding: 5px 7px;
    }
    .state {
      color: #4f5a5f;
      font-weight: 620;
    }
    .search-rank {
      justify-self: end;
      text-align: right;
    }
    .detail {
      border-top: 1px solid #e5e8e2;
      padding: 16px;
    }
    .detail-grid {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
      gap: 16px;
    }
    .detail-section {
      min-width: 0;
    }
    .detail-label {
      color: #687479;
      font-size: 12px;
      font-weight: 700;
      margin-bottom: 5px;
      text-transform: uppercase;
    }
    .detail-text {
      color: #263034;
      font-size: 14px;
      line-height: 1.45;
      overflow-wrap: anywhere;
      white-space: pre-wrap;
    }
    .detail-wide {
      grid-column: 1 / -1;
    }
    .detail-list {
      display: grid;
      gap: 8px;
      margin: 0;
      padding: 0;
      list-style: none;
    }
    .link-title {
      font-weight: 650;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      table-layout: fixed;
      background: white;
      border: 1px solid #d8ddd7;
    }
    th, td {
      text-align: left;
      vertical-align: top;
      border-bottom: 1px solid #e5e8e2;
      padding: 12px;
      font-size: 14px;
      line-height: 1.4;
      overflow-wrap: anywhere;
    }
    th {
      color: #576166;
      font-weight: 620;
      background: #eff2ed;
    }
    tr:last-child td {
      border-bottom: 0;
    }
    .title {
      font-weight: 650;
      margin-bottom: 4px;
    }
    .summary {
      color: #4f5a5f;
    }
    .meta {
      color: #687479;
      font-size: 12px;
      margin-top: 5px;
    }
    .signals {
      display: flex;
      flex-wrap: wrap;
      gap: 5px;
    }
    .signal {
      border: 1px solid #cfd6cf;
      background: #f5f7f3;
      border-radius: 999px;
      color: #3d474b;
      display: inline-flex;
      font-size: 12px;
      line-height: 1;
      padding: 5px 7px;
    }
    .score {
      font-variant-numeric: tabular-nums;
      white-space: nowrap;
    }
    .empty {
      border: 1px solid #d8ddd7;
      background: white;
      padding: 18px;
      color: #576166;
      font-size: 14px;
    }
    .pagination {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 10px;
      margin-top: 12px;
      color: #576166;
      font-size: 14px;
    }
    .page-link, .page-disabled {
      border: 1px solid #cfd6cf;
      border-radius: 6px;
      padding: 7px 10px;
      text-decoration: none;
    }
    .page-link {
      color: #283c32;
      background: white;
    }
    .page-disabled {
      color: #90999d;
      background: #f0f2ef;
    }
    @media (max-width: 820px) {
      main { padding: 24px 16px; }
      header, .section-heading, .search-options {
        align-items: flex-start;
        flex-direction: column;
      }
      .search-grid {
        grid-template-columns: 1fr;
      }
      .insight-summary {
        grid-template-columns: 1fr;
      }
      .vote-box {
        border-right: 0;
        border-bottom: 1px solid #e5e8e2;
        padding: 0 0 12px;
        text-align: left;
      }
      .vote-row {
        justify-content: flex-start;
      }
      .search-rank {
        justify-self: start;
        text-align: left;
      }
      .detail-grid {
        grid-template-columns: 1fr;
      }
      .pagination {
        justify-content: flex-start;
      }
    }
    @media (prefers-color-scheme: dark) {
      :root, body { background: #141819; color: #eef2ef; }
      header, table, th, td, .empty, .search-panel { border-color: #30383b; }
      table, .empty, .search-panel, .page-link { background: #1b2022; }
      th { background: #242b2e; color: #b4c0c3; }
      .summary, .count, label, .field-label, .meta { color: #b4c0c3; }
      .insight-card { background: #1b2022; border-color: #30383b; }
      .insight-card[open] { border-color: #566468; }
      .vote-box, .detail { border-color: #30383b; }
      .rank, .detail-text { color: #eef2ef; }
      .rank-label, .summary-line, .detail-label { color: #b4c0c3; }
      .badge { background: #242b2e; border-color: #3a4448; color: #d5dddf; }
      .state { color: #d5dddf; }
      input[type="search"], input[type="text"], input[type="number"] {
        background: #141819;
        border-color: #3a4448;
        color: #eef2ef;
      }
      button, .button-link { background: #8fb7a3; border-color: #8fb7a3; color: #111615; }
      .button-link.secondary { background: transparent; color: #cce0d4; }
      .check-row, .page-link { color: #cce0d4; }
      .signal { background: #242b2e; border-color: #3a4448; color: #d5dddf; }
      .page-disabled { background: #202629; border-color: #30383b; color: #7f898d; }
      .error { background: #331c1c; color: #ffc7c7; border-color: #7f3434; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>Consensus Admin</h1>
      <div class="count">{{.Recent.Total}} {{insightLabel .Recent.Total}}</div>
    </header>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}

    <section class="search-panel" aria-labelledby="search-heading">
      <div class="section-heading">
        <h2 id="search-heading">MCP Search</h2>
        {{if .Search.Searched}}<div class="count">{{len .SearchItems}} {{insightLabel (len .SearchItems)}}</div>{{end}}
      </div>
      <form method="get" action="/admin/">
        <input type="hidden" name="page_size" value="{{.Recent.PageSize}}">
        <div class="search-grid">
          <div class="field">
            <label for="query">Query</label>
            <input id="query" type="search" name="query" value="{{.Search.Query}}" placeholder="posthog sourcemaps upload duplicate commit">
          </div>
          <div class="field">
            <label for="tags">Tags</label>
            <input id="tags" type="text" name="tags" value="{{.Search.Tags}}" placeholder="posthog, source-maps, tool:turbo">
          </div>
          <div class="field">
            <label for="limit">Limit</label>
            <input id="limit" type="number" name="limit" min="0" max="50" value="{{.Search.Limit}}">
          </div>
        </div>
        <div class="search-options">
          <label class="check-row" for="include_upstreams">
            <input id="include_upstreams" type="checkbox" name="include_upstreams" value="true" {{if .Search.IncludeUpstreams}}checked{{end}}>
            Include upstreams
          </label>
          <div class="actions">
            <button type="submit">Search</button>
            <a class="button-link secondary" href="{{.ClearSearchURL}}">Clear</a>
          </div>
        </div>
      </form>
    </section>

    {{if .Search.Searched}}
    <section aria-labelledby="results-heading">
      <div class="section-heading">
        <h2 id="results-heading">Search Results</h2>
      </div>
      {{if .Search.Error}}
      <div class="error">{{.Search.Error}}</div>
      {{else if .SearchItems}}
      <div class="insight-list">
        {{range .SearchItems}}{{template "insightCard" .}}{{end}}
      </div>
      {{else}}
      <div class="empty">No search results.</div>
      {{end}}
    </section>
    {{end}}

    <section aria-labelledby="recent-heading">
      <div class="section-heading">
        <h2 id="recent-heading">Recent Insights</h2>
        <div class="count">Page {{.Recent.Page}} of {{.Recent.TotalPages}}</div>
      </div>
    {{if .RecentItems}}
    <div class="insight-list">
      {{range .RecentItems}}{{template "insightCard" .}}{{end}}
    </div>
    {{if gt .Recent.TotalPages 1}}
    <nav class="pagination" aria-label="Recent insights pages">
      {{if .PreviousPageURL}}<a class="page-link" href="{{.PreviousPageURL}}">Previous</a>{{else}}<span class="page-disabled">Previous</span>{{end}}
      <span>Page {{.Recent.Page}} of {{.Recent.TotalPages}}</span>
      {{if .NextPageURL}}<a class="page-link" href="{{.NextPageURL}}">Next</a>{{else}}<span class="page-disabled">Next</span>{{end}}
    </nav>
    {{end}}
    {{else}}
    <div class="empty">No insights yet.</div>
    {{end}}
    </section>
  </main>
</body>
</html>`))

func requestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mcp") && r.Header.Get("Origin") != "" {
			w.Header().Set("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}
