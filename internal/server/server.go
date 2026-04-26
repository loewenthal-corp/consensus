package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

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

	registerConnectHandlers(mux, cfg.Service)

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

func registerConnectHandlers(mux *http.ServeMux, svc *consensus.Service) {
	path, handler := consensusv1connect.NewInsightServiceHandler(svc)
	mux.Handle(path, handler)
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

const mcpInstructions = `Consensus is market-based context engineering for AI agents.

Use it as a shared organizational memory layer for compact, evidence-backed insights that should survive one agent thread. Search before rediscovering a solution. Create only durable insights with the situation, answer, concrete action, optional example, and useful links. Record outcomes after an insight actually solved, helped, failed after being applied, or became stale so ranking can learn from real outcomes.

The MCP surface is intentionally tiny: search, get, create, and record_outcome. Links carry docs, source threads, related insights, issues, and evidence as fields on insights. Administrative edits and broader API operations belong on the API/admin port, not in agent tools.`

var mcpToolComments = map[protoreflect.FullName]string{
	"consensus.v1.InsightService.Search":        `Search returns ranked insights for a problem statement, exact error, failing command, stack trace, snippet, or repo/tool context. Include the smallest concrete example available when it helps identify the issue.`,
	"consensus.v1.InsightService.Get":           `Get returns one insight by local ID or federated reference, including the situation, answer, action, optional example, links, lifecycle state, and review state.`,
	"consensus.v1.InsightService.Create":        `Create submits a new candidate insight after a thread discovers a durable answer. Store the distilled situation, answer, concrete action, optional example, and useful links instead of pasting a whole conversation.`,
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
		s.AddTool(tool, func(ctx context.Context, request *runtime.CallToolRequest) (*runtime.CallToolResult, error) {
			marshaled, err := json.Marshal(request.Arguments)
			if err != nil {
				return nil, err
			}
			req := gen.DynamicNewMessage(md.Input())
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(marshaled, req); err != nil {
				return nil, err
			}

			resp, err := handler(ctx, md, req)
			if err != nil {
				return runtime.HandleError(err)
			}

			marshaled, err = (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(resp)
			if err != nil {
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
		insights, err := svc.ListRecentInsights(r.Context(), 25)
		data := adminPageData{Insights: insights}
		if err != nil {
			data.Error = err.Error()
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := adminTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type adminPageData struct {
	Insights []*consensusv1.Insight
	Error    string
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
    .error {
      border: 1px solid #c95252;
      background: #fff0f0;
      color: #8d1f1f;
      padding: 10px 12px;
      margin-bottom: 16px;
      border-radius: 6px;
      font-size: 14px;
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
    .empty {
      border: 1px solid #d8ddd7;
      background: white;
      padding: 18px;
      color: #576166;
      font-size: 14px;
    }
    @media (prefers-color-scheme: dark) {
      :root, body { background: #141819; color: #eef2ef; }
      header, table, th, td, .empty { border-color: #30383b; }
      table, .empty { background: #1b2022; }
      th { background: #242b2e; color: #b4c0c3; }
      .summary, .count { color: #b4c0c3; }
      .error { background: #331c1c; color: #ffc7c7; border-color: #7f3434; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>Consensus Admin</h1>
      <div class="count">{{len .Insights}} {{insightLabel (len .Insights)}}</div>
    </header>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
    {{if .Insights}}
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
        {{range .Insights}}
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
    {{else}}
    <div class="empty">No insights yet.</div>
    {{end}}
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
