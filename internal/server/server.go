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

const mcpInstructions = `Consensus is a small MCP component for reusing durable insights from prior agent work.

Use it like grep or web search: search when a previous answer might save work; if a result applies, get it or follow its links; after applying it, record the outcome. Create only compact issue/answer/action insights with evidence.

Consensus is not a workflow engine and should not run a back-and-forth research loop for the agent. The MCP surface is intentionally tiny: search, get, create, and record_outcome.`

var mcpToolComments = map[protoreflect.FullName]string{
	"consensus.v1.InsightService.Search":        `Search returns ranked insights for a concrete problem, exact error, failing command, stack trace, snippet, or repo/tool context. Use it as a one-shot retrieval tool before rediscovering a known issue; follow returned links or fetch a matching insight when useful.`,
	"consensus.v1.InsightService.Get":           `Get returns one insight by local ID or federated reference, including the situation, answer, action, optional example, links, lifecycle state, and review state.`,
	"consensus.v1.InsightService.Create":        `Create submits a compact candidate insight after a thread discovers a durable answer. This is not a note-for-later or transcript store; include the situation, answer, action, optional example, and useful links.`,
	"consensus.v1.InsightService.RecordOutcome": `RecordOutcome records whether an insight worked after it was applied. Use did_not_work only when the insight appeared to match the problem and the suggested action was tried but failed, not when the result was merely irrelevant.`,
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
	return mcpToolComments[method.FullName()]
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
		}
		if recent.Page > 1 {
			data.PreviousPageURL = adminPageURL(r, recent.Page-1)
		}
		if recent.Total > 0 && recent.Page < recent.TotalPages {
			data.NextPageURL = adminPageURL(r, recent.Page+1)
		}

		if strings.TrimSpace(data.Search.Query) != "" {
			data.Search.Searched = true
			contextFilters, err := parseAdminSearchContext(data.Search.Context)
			if err != nil {
				data.Search.Error = err.Error()
			} else {
				resp, err := svc.Search(r.Context(), &consensusv1.InsightServiceSearchRequest{
					Query:            data.Search.Query,
					Tags:             parseAdminSearchTags(data.Search.Tags),
					Context:          contextFilters,
					Limit:            int32(data.Search.Limit),
					IncludeUpstreams: data.Search.IncludeUpstreams,
				})
				if err != nil {
					data.Search.Error = err.Error()
				} else {
					data.Search.Results = resp.GetResults()
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
	Search          adminSearchData
	PreviousPageURL string
	NextPageURL     string
	ClearSearchURL  string
	Error           string
}

type adminSearchData struct {
	Query            string
	Tags             string
	Context          string
	Limit            int
	IncludeUpstreams bool
	Searched         bool
	Results          []*consensusv1.InsightSearchResult
	Error            string
}

func adminSearchFromRequest(r *http.Request) adminSearchData {
	values := r.URL.Query()
	return adminSearchData{
		Query:            strings.TrimSpace(values.Get("query")),
		Tags:             values.Get("tags"),
		Context:          values.Get("context"),
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
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func parseAdminSearchContext(raw string) (map[string]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make(map[string]string, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("context filters must use key=value entries")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, fmt.Errorf("context filters must use non-empty key=value entries")
		}
		out[key] = value
	}
	return out, nil
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
	for _, key := range []string{"query", "tags", "context", "limit", "include_upstreams", "page"} {
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
}).Parse(`<!doctype html>
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
        {{if .Search.Searched}}<div class="count">{{len .Search.Results}} {{insightLabel (len .Search.Results)}}</div>{{end}}
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
            <input id="tags" type="text" name="tags" value="{{.Search.Tags}}" placeholder="posthog, source-maps">
          </div>
          <div class="field">
            <label for="context">Context</label>
            <input id="context" type="text" name="context" value="{{.Search.Context}}" placeholder="tool=turbo">
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
      {{else if .Search.Results}}
      <table>
        <thead>
          <tr>
            <th style="width: 37%">Insight</th>
            <th style="width: 10%">Score</th>
            <th style="width: 16%">Signals</th>
            <th style="width: 23%">Rank Reason</th>
            <th style="width: 14%">Updated</th>
          </tr>
        </thead>
        <tbody>
          {{range .Search.Results}}
          <tr>
            <td>
              <div class="title">{{.Insight.Title}}</div>
              <div class="summary">{{.Insight.Answer}}</div>
              <div class="meta">{{.Insight.Id}} / {{.Insight.Kind}}</div>
            </td>
            <td><span class="score">{{printf "%.6f" .Score}}</span></td>
            <td>
              <div class="signals">
                {{range .MatchedSignals}}<span class="signal">{{.}}</span>{{end}}
              </div>
            </td>
            <td>{{.RankReason}}</td>
            <td>{{formatTime .Insight.UpdatedAt}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
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
    {{if .Recent.Insights}}
    <table>
      <thead>
        <tr>
          <th style="width: 40%">Insight</th>
          <th style="width: 14%">Kind</th>
          <th style="width: 14%">State</th>
          <th style="width: 18%">Tags</th>
          <th style="width: 14%">Updated</th>
        </tr>
      </thead>
      <tbody>
        {{range .Recent.Insights}}
        <tr>
          <td>
            <div class="title">{{.Title}}</div>
            <div class="summary">{{.Answer}}</div>
          </td>
          <td>{{.Kind}}</td>
          <td>{{.ReviewState}} / {{.LifecycleState}}</td>
          <td>{{join .Tags ", "}}</td>
          <td>{{formatTime .UpdatedAt}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
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
