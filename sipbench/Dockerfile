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
# tc and ip impair the runner's own traffic for the netem scenarios, and need NET_ADMIN, which only the runner's
# container is given (compose.yaml, k8s/runner-job.yaml). The runner, as the non-root bench user, holds it through
# a file capability (permitted only, so it starts the same without NET_ADMIN) and passes it to tc and ip as an
# ambient capability; ip ignores file capabilities on itself.
RUN apk add --no-cache iproute2 libcap-setcap
RUN adduser -D -u 10001 bench
COPY --from=build /out/ /usr/local/bin/
RUN setcap cap_net_admin+p /usr/local/bin/sipbench && apk del --no-cache libcap-setcap
COPY backends /bench/backends
COPY scenarios /bench/scenarios
WORKDIR /bench
USER bench
ENTRYPOINT ["sipbench"]
CMD ["run", "scenarios"]
