FROM docker.io/library/golang:1.27.3-alpine AS build

ARG VERSION=""
ARG REVISION=""

WORKDIR /src
COPY . .
# vendor/ is not in Git, so a build from a clone writes it here. With a vendor
# tree in the context, the build stays offline.
RUN [ -f vendor/modules.txt ] || go mod vendor
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath \
        -ldflags="-s -w \
          -X github.com/bespinian/keera-gateway/internal/version.release=${VERSION} \
          -X github.com/bespinian/keera-gateway/internal/version.revision=${REVISION}" \
        -o /out/ ./cmd/keera-gateway
RUN sh scripts/third-party-notices.sh > /out/THIRD_PARTY_NOTICES

FROM scratch

LABEL org.opencontainers.image.title="Keera Gateway" \
      org.opencontainers.image.vendor="bespinian GmbH" \
      org.opencontainers.image.licenses="LicenseRef-Keera-Community-1.0" \
      org.opencontainers.image.source="https://github.com/bespinian/keera-gateway"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/keera-gateway /keera-gateway

COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_NOTICES /THIRD_PARTY_NOTICES

# nobody:nogroup, numeric as there is no /etc/passwd.
USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/keera-gateway"]
CMD ["serve"]
