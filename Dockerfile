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
# It is baked in by default: an image that needs a signup and a manual download
# before it can report a country is not a working default.
#
# Country and ASN only. Those are the two geo dimensions enabled by default,
# and together they cost ~18 MB. The city database is another ~131 MB on a
# ~20 MB image, and city is opt-in anyway, so it is opt-in here too:
#   docker build --build-arg WITH_GEO_CITY=1 .
# Build with --build-arg WITH_GEO=0 for an image with no geo data at all.
FROM alpine:3.24 AS geo
ARG WITH_GEO=1
ARG WITH_GEO_CITY=0
RUN set -eu; \
    mkdir -p /geo; \
    [ "$WITH_GEO" = "1" ] || exit 0; \
    apk add --no-cache curl; \
    # DB-IP publishes mid-month, so on the 1st the current month's file does
    # not exist yet. Falling back to the previous month keeps a release cut on
    # an unlucky day from failing the build outright.
    y=$(date +%Y); m=$(date +%m); \
    pm=$((10#$m - 1)); py=$y; \
    if [ "$pm" -eq 0 ]; then pm=12; py=$((y - 1)); fi; \
    prev=$(printf '%04d-%02d' "$py" "$pm"); \
    fetch() { \
      for month in "$(date +%Y-%m)" "$prev"; do \
        if curl -fsSL -o /tmp/db.gz "https://download.db-ip.com/free/$1-${month}.mmdb.gz"; then \
          gunzip -c /tmp/db.gz > "/geo/$1.mmdb"; \
          rm -f /tmp/db.gz; \
          return 0; \
        fi; \
      done; \
      echo "could not download $1 for $(date +%Y-%m) or $prev" >&2; \
      return 1; \
    }; \
    fetch dbip-country-lite; \
    fetch dbip-asn-lite; \
    if [ "$WITH_GEO_CITY" = "1" ]; then fetch dbip-city-lite; fi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ridiculytics /ridiculytics
# Baked databases live under /usr/share: read-only data that ships with the
# image. /var/lib/ridiculytics is left free as the mount point for operator
# supplied databases, which are preferred when present because they are the
# ones that can be refreshed without rebuilding. Baking into the mount point
# instead would mean any `-v ...:/var/lib/ridiculytics` silently hid them.
COPY --from=geo /geo /usr/share/ridiculytics/geo
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
