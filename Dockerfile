ARG GOPROXY=https://repo.snapp.tech/repository/goproxy/,direct

FROM registry.snapp.tech/docker/golang:1.25-alpine AS build

ARG VERSION=dev
ARG GOPROXY
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build \
    -ldflags="-X gitlab.snapp.ir/snappcloud/openshift-mcp/internal/version.Version=${VERSION}" \
    -o /bin/openshift-mcp ./cmd/openshift-mcp

FROM alpine:3.23

RUN adduser -D -u 10001 appuser
COPY --from=build /bin/openshift-mcp /openshift-mcp
USER 10001

EXPOSE 8080

ENTRYPOINT ["/openshift-mcp"]
