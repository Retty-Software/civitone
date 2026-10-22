# Civitone controller with snapserver: one container per site.
#
#   docker build -t civitone .
#   docker run -d -p 8080:8080 -p 1704:1704 -v civitone:/data \
#     -e CIVITONE_ADMIN_KEY=$(openssl rand -hex 32) \
#     -e CIVITONE_PUBLIC_URL=https://speakers.example.com \
#     -e CIVITONE_SNAP_HOST=speakers.example.com civitone
#
# Port 8080 is the console, /node and /source (put HTTPS in front of it);
# 1704 is the audio stream the poles play. Everything lives in /data.
# See deploy/docker/README.md.

FROM golang:1.24-bookworm AS build
WORKDIR /src/controller
COPY controller/go.mod controller/go.sum ./
RUN go mod download
COPY controller/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/civitone ./cmd/civitone

FROM debian:trixie-slim
ARG SNAP_VERSION=0.35.0
ARG TARGETARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && curl -fsSL -o /tmp/snapserver.deb \
      "https://github.com/badaix/snapcast/releases/download/v${SNAP_VERSION}/snapserver_${SNAP_VERSION}-1_${TARGETARCH}_trixie.deb" \
 && apt-get install -y --no-install-recommends /tmp/snapserver.deb \
 && rm /tmp/snapserver.deb \
 && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home /data --shell /usr/sbin/nologin civitone \
 && install -d -o civitone -g civitone /data /etc/civitone

# The development config, with the cloud differences: snapserver's own
# HTTP/JSON-RPC stays inside the container, a 2 s buffer absorbs internet
# jitter, and its state lives in /data.
COPY controller/snapserver.conf /tmp/snapserver.conf
RUN sed -e 's/^bind_to_address = 0\.0\.0\.0$/bind_to_address = 127.0.0.1/' \
        -e 's/^buffer = .*/buffer = 2000/' /tmp/snapserver.conf > /etc/civitone/snapserver.conf \
 && printf '\n[server]\ndatadir = /data/snapserver\n' >> /etc/civitone/snapserver.conf \
 && rm /tmp/snapserver.conf

COPY --from=build /out/civitone /usr/local/bin/civitone
USER civitone
ENV HOME=/data
VOLUME /data
EXPOSE 8080 1704
ENTRYPOINT ["civitone", "--listen", ":8080", "--db", "/data/civitone.db", \
  "--snapserver", "/usr/bin/snapserver", "--snapserver-config", "/etc/civitone/snapserver.conf", \
  "--snapserver-runtime-config", "/data/snapserver.run.conf", "--snap-buffer-ms", "2000"]
