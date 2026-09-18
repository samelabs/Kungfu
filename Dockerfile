# Kungfu — single production container contract.
# Same Git commit: apply migrations/*.sql first (outside this image),
# then run this image. The container does not migrate.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/kungfu-server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -o /out/kungfu-adminctl ./cmd/adminctl

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && addgroup -g 65532 -S kungfu \
 && adduser -u 65532 -S -G kungfu -H -D kungfu
COPY --from=build /out/kungfu-server /usr/local/bin/kungfu-server
COPY --from=build /out/kungfu-adminctl /usr/local/bin/kungfu-adminctl
USER 65532:65532
ENV LISTEN_ADDR=0.0.0.0:8090
EXPOSE 8090
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/kungfu-server"]
