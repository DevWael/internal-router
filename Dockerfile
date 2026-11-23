FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN go vet ./... && go test ./... \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/internal-router . \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/internal-router /usr/local/bin/internal-router
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV ROUTER_DATA=/data
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["internal-router", "health"]
ENTRYPOINT ["internal-router"]
CMD ["serve"]
