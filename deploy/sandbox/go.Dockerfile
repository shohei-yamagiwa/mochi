# Container snapshot for reviewing Go repositories, including Mochi itself.
# Create separately as mochi-review-go; retain mochi-review for rollback.
FROM golang:1.27.4-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git bash curl ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
