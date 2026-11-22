FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" -o /out/oci-mirror ./cmd/oci-mirror \
    && install -d -o 65532 -g 65532 /out/cache \
    && install -d -m 1777 /out/tmp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/oci-mirror /oci-mirror
COPY --from=build --chown=65532:65532 /out/cache /var/lib/oci-mirror/cache
COPY --from=build /out/tmp /tmp
USER 65532:65532
ENTRYPOINT ["/oci-mirror"]
