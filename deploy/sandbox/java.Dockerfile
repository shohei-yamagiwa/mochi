# Container snapshot for reviewing Java repositories (BioCold Nexus).
# Build as a Daytona snapshot named `mochi-review-java`.
FROM eclipse-temurin:21-jdk-jammy
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git bash curl ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /workspace
