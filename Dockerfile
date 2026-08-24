FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/zync-hub ./cmd/zync-hub \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/zync ./cmd/zync

FROM alpine:3.22
# git serves the repos; curl is used by the pre-receive fencing hook
RUN apk add --no-cache git curl ca-certificates
COPY --from=build /out/zync-hub /usr/local/bin/zync-hub
COPY --from=build /out/zync /usr/local/bin/zync
ENV ZYNC_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["zync-hub"]
