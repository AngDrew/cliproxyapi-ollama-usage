FROM golang:1.26.10-bookworm AS build
WORKDIR /src
COPY go.mod *.go ./
RUN go test -race ./... && go vet ./...
ARG VERSION=0.1.0
RUN mkdir /out && CGO_ENABLED=1 GOAMD64=v1 GOARM64=v8.0 go build -buildmode=c-shared -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ollama-usage.so .
FROM scratch AS artifact
COPY --from=build /out/ollama-usage.so /
