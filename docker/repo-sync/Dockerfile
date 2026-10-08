FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /repo-sync .

FROM alpine:3.23
RUN apk add --no-cache git ca-certificates openssh-client
COPY known_hosts /etc/repo-sync/known_hosts
COPY --from=build /repo-sync /usr/local/bin/repo-sync
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/repo-sync"]
