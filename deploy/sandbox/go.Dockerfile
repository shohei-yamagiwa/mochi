# Container snapshot for reviewing Go repositories, including Mochi itself.
# Build as a Daytona snapshot named `mochi-review-go` for the full preset.
FROM golang:1.27.4-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git bash curl ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
