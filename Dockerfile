FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /ridiculytics ./cmd/ridiculytics

# DB-IP Lite is CC-BY-4.0, so unlike GeoLite2 it may ship inside the image.
# Download it at build time to keep a working geo setup with no signup:
#   docker build --build-arg WITH_GEO=1 .
FROM alpine:3.20 AS geo
ARG WITH_GEO=0
RUN mkdir -p /geo && if [ "$WITH_GEO" = "1" ]; then \
      apk add --no-cache curl && \
      MONTH=$(date +%Y-%m) && \
      curl -fsSL "https://download.db-ip.com/free/dbip-city-lite-${MONTH}.mmdb.gz" | gunzip > /geo/dbip-city-lite.mmdb && \
      curl -fsSL "https://download.db-ip.com/free/dbip-asn-lite-${MONTH}.mmdb.gz"  | gunzip > /geo/dbip-asn-lite.mmdb ; \
    fi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ridiculytics /ridiculytics
COPY --from=geo /geo /var/lib/ridiculytics
# Shipped as a reference only, never as the default config. Defaulting to it
# would mean an image started with no configuration silently comes up tracking
# the placeholder domain inside it instead of failing.
COPY sites.yaml /etc/ridiculytics/sites.yaml.example

# 8080 is public ingest. 9090 is metrics and should stay on a private network.
EXPOSE 8080 9090
USER nonroot:nonroot

# No arguments: configuration comes from RIDICULYTICS_* environment variables.
# Mount a file and pass -config to use YAML instead.
ENTRYPOINT ["/ridiculytics"]
CMD []
