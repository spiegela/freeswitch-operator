# One image with both bench binaries: sipbench (the runner) and mockbackend.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sipbench ./cmd/sipbench \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockbackend ./cmd/mockbackend

FROM alpine:3.22
RUN adduser -D -u 10001 bench
COPY --from=build /out/ /usr/local/bin/
COPY backends /bench/backends
COPY scenarios /bench/scenarios
WORKDIR /bench
USER bench
ENTRYPOINT ["sipbench"]
CMD ["run", "scenarios"]
