# Nimbus Functions — Go runtime (provided-style)
# The image only has the platform bootstrap. The user's zip mounts a static
# linux binary at /var/task (default name: bootstrap). Source is not compiled
# here: the container has network=none and no module cache.

FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod bootstrap.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bootstrap .

FROM alpine:3.21

RUN adduser -D -u 1000 -h /tmp node
COPY --from=build /bootstrap /var/runtime/bootstrap

ENV NIMBUS_HANDLER_PATH=/var/task/bootstrap \
    NIMBUS_EVENT_PATH=/var/task/event.json

ENTRYPOINT ["/var/runtime/bootstrap"]
