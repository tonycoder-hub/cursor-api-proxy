FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY gen ./gen
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cursor-api-proxy ./cmd/cursor-api-proxy
RUN mkdir -p /workspace

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/cursor-api-proxy /cursor-api-proxy
COPY --from=builder /workspace /workspace
WORKDIR /workspace
USER 65532:65532
ENV LISTEN_ADDR=0.0.0.0:8787
EXPOSE 8787
ENTRYPOINT ["/cursor-api-proxy"]
