# Build as a Daytona snapshot named `mochi-review` (separate from the server).
# Add the target repository's language runtimes here if its tests need them.
FROM ubuntu:24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git bash curl ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
