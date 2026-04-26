package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	mux.HandleFunc("GET /admin/", handleAdmin)

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
	raw, mcpServer := gosdk.NewServer("consensus", "0.0.0")
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
			Provider: runtime.LLMProviderStandard,
		})
	}

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return raw
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
	}), nil
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

func handleAdmin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><title>Consensus Admin</title><main><h1>Consensus Admin</h1><p>Knowledge review and operations will live here.</p></main>")
}

func requestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/mcp") && r.Header.Get("Origin") != "" {
			w.Header().Set("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}
