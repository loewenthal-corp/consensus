package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var insightExchangeTracer = otel.Tracer("github.com/loewenthal-corp/consensus/internal/server")

func newInsightExchangeLoggingInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			resp, err := next(ctx, req)

			if !logger.Enabled(ctx, slog.LevelDebug) {
				return resp, err
			}

			attrs := insightExchangeAttrs(ctx, "connect", start, err,
				slog.String("procedure", req.Spec().Procedure),
				slog.String("protocol", req.Peer().Protocol),
			)
			if err != nil {
				attrs = append(attrs, slog.String("code", connect.CodeOf(err).String()))
			}
			if msg, ok := req.Any().(proto.Message); ok {
				attrs = append(attrs, slog.Any("request", protoLogValue(msg)))
			}
			if msg := connectResponseProto(resp); msg != nil {
				attrs = append(attrs, slog.Any("response", protoLogValue(msg)))
			}

			logger.LogAttrs(ctx, slog.LevelDebug, "insight exchange", attrs...)
			return resp, err
		}
	}
}

func beginMCPInsightExchange(ctx context.Context, method protoreflect.MethodDescriptor, toolName string) (context.Context, func(proto.Message, proto.Message, any, error)) {
	methodName := string(method.FullName())
	ctx, span := insightExchangeTracer.Start(ctx, methodName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("rpc.system", "mcp"),
			attribute.String("rpc.service", string(method.Parent().FullName())),
			attribute.String("rpc.method", string(method.Name())),
			attribute.String("mcp.tool", toolName),
		),
	)
	start := time.Now()

	return ctx, func(req proto.Message, resp proto.Message, rawRequest any, err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}

		logger := slog.Default()
		if logger.Enabled(ctx, slog.LevelDebug) {
			attrs := insightExchangeAttrs(ctx, "mcp", start, err,
				slog.String("method", methodName),
				slog.String("tool", toolName),
			)
			if req != nil {
				attrs = append(attrs, slog.Any("request", protoLogValue(req)))
			} else if rawRequest != nil {
				attrs = append(attrs, slog.Any("request", rawRequest))
			}
			if resp != nil {
				attrs = append(attrs, slog.Any("response", protoLogValue(resp)))
			}
			logger.LogAttrs(ctx, slog.LevelDebug, "insight exchange", attrs...)
		}

		span.End()
	}
}

func connectResponseProto(resp connect.AnyResponse) proto.Message {
	if resp == nil {
		return nil
	}
	value := reflect.ValueOf(resp)
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return nil
	}
	msg, _ := resp.Any().(proto.Message)
	return msg
}

func insightExchangeAttrs(ctx context.Context, transport string, start time.Time, err error, attrs ...slog.Attr) []slog.Attr {
	duration := time.Since(start)
	out := []slog.Attr{
		slog.String("component", "consensus.insight_exchange"),
		slog.String("transport", transport),
		slog.String("exchange_id", uuid.NewString()),
		slog.Duration("duration", duration),
		slog.Int64("duration_ms", duration.Milliseconds()),
	}
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.HasTraceID() {
		out = append(out, slog.String("trace_id", spanCtx.TraceID().String()))
	}
	if err != nil {
		out = append(out,
			slog.String("outcome", "error"),
			slog.String("error", err.Error()),
		)
	} else {
		out = append(out, slog.String("outcome", "success"))
	}
	return append(out, attrs...)
}

func protoLogValue(msg proto.Message) any {
	if msg == nil {
		return nil
	}

	body, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(msg)
	if err != nil {
		return fmt.Sprintf("proto marshal error: %v", err)
	}

	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return string(body)
	}
	return value
}
