# Build from the repo root: docker build -f cmd/rendezvous/Dockerfile -t wifi-cursor-rendezvous .
FROM golang:latest AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/id ./internal/id
COPY internal/protocol ./internal/protocol
COPY cmd/rendezvous ./cmd/rendezvous
RUN CGO_ENABLED=0 go build -o /rendezvous ./cmd/rendezvous

FROM alpine:latest
COPY --from=build /rendezvous /usr/local/bin/rendezvous
EXPOSE 47990
ENTRYPOINT ["/usr/local/bin/rendezvous"]
