# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/consensus ./cmd/consensus

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S consensus \
    && adduser -S -G consensus consensus

COPY --from=build /out/consensus /usr/local/bin/consensus

USER consensus
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/consensus"]
