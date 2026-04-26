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

func New(cfg Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, fmt.Errorf("service is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /admin/", handleAdmin(cfg.Service))

	registerConnectHandlers(mux, cfg.Service)

	mcpHandler, err := newMCPHandler(cfg.Service)
	if err != nil {
		return nil, err
	}
	mux.Handle("/mcp", mcpHandler)

	return requestMiddleware(mux), nil
}

func registerConnectHandlers(mux *http.ServeMux, svc *consensus.Service) {
	path, handler := consensusv1connect.NewKnowledgeServiceHandler(svc)
	mux.Handle(path, handler)
	path, handler = consensusv1connect.NewVoteServiceHandler(svc)
	mux.Handle(path, handler)
	path, handler = consensusv1connect.NewGraphServiceHandler(svc)
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
		"consensus.v1.KnowledgeService",
		"consensus.v1.VoteService",
		"consensus.v1.GraphService",
	} {
		sd, err := findServiceDescriptor(serviceName)
		if err != nil {
			return nil, err
		}
		gen.RegisterService(mcpServer, sd, handler, gen.RegisterServiceOptions{
			Provider:        runtime.LLMProviderStandard,
			CommentProvider: mcpToolComment,
		})
	}

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return raw
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
	}), nil
}

const mcpInstructions = `Consensus is market-based context engineering for AI agents.

Use it as a shared organizational memory layer for compact, evidence-backed learnings that should survive one agent thread. Search before rediscovering a solution. Contribute only durable findings with problem context, concrete action, and evidence. Cast votes after a unit actually solved, helped, failed, or became stale so ranking can learn from real outcomes.

Read operations return knowledge units and graph relationships. Write operations mutate shared memory and are intended for trusted deployments or scoped authorization.`

var mcpToolComments = map[protoreflect.FullName]string{
	"consensus.v1.KnowledgeService.Search":     `Search returns ranked knowledge units for a problem statement, error, task, failing command, stack trace, or repo/tool context. Use this before spending tokens rediscovering an answer that another agent may have already proven.`,
	"consensus.v1.KnowledgeService.Get":        `Get returns one knowledge unit by ID, including its answer, action, labels, context, evidence references, lifecycle state, and review state.`,
	"consensus.v1.KnowledgeService.Contribute": `Contribute submits a new candidate knowledge unit after a thread discovers a durable lesson. Store the distilled problem, context, answer, concrete action, and evidence instead of pasting a whole conversation.`,
	"consensus.v1.KnowledgeService.Update":     `Update amends an existing knowledge unit when the stored finding needs a clearer title, fresher evidence, corrected context, or a revised action.`,
	"consensus.v1.VoteService.Cast":            `Cast records a utility signal for a knowledge unit. Use outcomes such as solved, helped, failed, or stale to teach ranking what actually worked in the current environment.`,
	"consensus.v1.VoteService.Retract":         `Retract removes or corrects a previous utility signal when the vote was accidental, duplicated, or no longer represents the observed result.`,
	"consensus.v1.GraphService.Link":           `Link creates a typed relationship between two knowledge or problem nodes, such as related, same_root_cause, supersedes, requires, or contradicts.`,
	"consensus.v1.GraphService.Unlink":         `Unlink tombstones a relationship when a graph edge was wrong, duplicated, or no longer useful for retrieval.`,
	"consensus.v1.GraphService.Neighbors":      `Neighbors returns nearby units and relationships so an agent can inspect adjacent issues, solution clusters, and root-cause neighborhoods.`,
	"consensus.v1.GraphService.ExplainPath":    `ExplainPath explains why two units or problems are connected, returning the graph edges that justify the relationship when a path is known.`,
}

func mcpToolComment(method protoreflect.MethodDescriptor) string {
	if location := method.ParentFile().SourceLocations().ByDescriptor(method); strings.TrimSpace(location.LeadingComments) != "" {
		return location.LeadingComments
	}
	return mcpToolComments[method.FullName()]
}

func serviceDispatcher(svc *consensus.Service) gen.Handler {
	return func(ctx context.Context, method protoreflect.MethodDescriptor, req proto.Message) (proto.Message, error) {
		switch string(method.FullName()) {
		case "consensus.v1.KnowledgeService.Search":
			var concrete consensusv1.KnowledgeServiceSearchRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Search(ctx, &concrete)
		case "consensus.v1.KnowledgeService.Get":
			var concrete consensusv1.KnowledgeServiceGetRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Get(ctx, &concrete)
		case "consensus.v1.KnowledgeService.Contribute":
			var concrete consensusv1.KnowledgeServiceContributeRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Contribute(ctx, &concrete)
		case "consensus.v1.KnowledgeService.Update":
			var concrete consensusv1.KnowledgeServiceUpdateRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Update(ctx, &concrete)
		case "consensus.v1.VoteService.Cast":
			var concrete consensusv1.VoteServiceCastRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Cast(ctx, &concrete)
		case "consensus.v1.VoteService.Retract":
			var concrete consensusv1.VoteServiceRetractRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Retract(ctx, &concrete)
		case "consensus.v1.GraphService.Link":
			var concrete consensusv1.GraphServiceLinkRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Link(ctx, &concrete)
		case "consensus.v1.GraphService.Unlink":
			var concrete consensusv1.GraphServiceUnlinkRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Unlink(ctx, &concrete)
		case "consensus.v1.GraphService.Neighbors":
			var concrete consensusv1.GraphServiceNeighborsRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.Neighbors(ctx, &concrete)
		case "consensus.v1.GraphService.ExplainPath":
			var concrete consensusv1.GraphServiceExplainPathRequest
			if err := remarshal(req, &concrete); err != nil {
				return nil, err
			}
			return svc.ExplainPath(ctx, &concrete)
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
		units, err := svc.ListRecentKnowledge(r.Context(), 25)
		data := adminPageData{Units: units}
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
	Units []*consensusv1.KnowledgeUnit
	Error string
}

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"join": strings.Join,
	"unitLabel": func(count int) string {
		if count == 1 {
			return "knowledge unit"
		}
		return "knowledge units"
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
      <div class="count">{{len .Units}} {{unitLabel (len .Units)}}</div>
    </header>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
    {{if .Units}}
    <table>
      <thead>
        <tr>
          <th style="width: 40%">Knowledge</th>
          <th style="width: 14%">Kind</th>
          <th style="width: 14%">State</th>
          <th style="width: 18%">Labels</th>
          <th style="width: 14%">Updated</th>
        </tr>
      </thead>
      <tbody>
        {{range .Units}}
        <tr>
          <td>
            <div class="title">{{.Title}}</div>
            <div class="summary">{{.Summary}}</div>
          </td>
          <td>{{.Kind}}</td>
          <td>{{.ReviewState}} / {{.LifecycleState}}</td>
          <td>{{join .Labels ", "}}</td>
          <td>{{formatTime .UpdatedAt}}</td>
        </tr>
        {{end}}
      </tbody>
    </table>
    {{else}}
    <div class="empty">No knowledge units yet.</div>
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
