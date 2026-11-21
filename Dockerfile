FROM golang:alpine AS build
RUN apk add --no-cache curl jq
WORKDIR /src
COPY . .
RUN sh tools/prepare.sh

FROM build AS tests
RUN apk add --no-cache build-base
RUN go test -mod=readonly -race ./... && go vet -mod=readonly ./...

FROM build AS binary
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /emulator ./cmd/emulator

FROM alpine:latest AS runtime
RUN apk add --no-cache ca-certificates && addgroup -S emulator && adduser -S -G emulator emulator && mkdir /data && chown emulator:emulator /data && chmod 700 /data
COPY --from=binary /emulator /usr/local/bin/emulator
USER emulator
EXPOSE 51080 51081
VOLUME ["/data"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s CMD ["emulator", "healthcheck"]
ENTRYPOINT ["emulator"]
CMD ["-listen", ":51080", "-control-listen", ":51081"]
