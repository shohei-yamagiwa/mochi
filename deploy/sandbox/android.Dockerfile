# Container snapshot for reviewing Android (Kotlin/Jetpack Compose) repositories.
# Build as a Daytona snapshot named `mochi-review-android`.
FROM eclipse-temurin:17-jdk-jammy
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git bash curl ripgrep unzip \
    && rm -rf /var/lib/apt/lists/*
ENV ANDROID_HOME=/opt/android-sdk
RUN mkdir -p "$ANDROID_HOME/cmdline-tools" \
    && curl -sSL https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip -o /tmp/cmdtools.zip \
    && unzip -q /tmp/cmdtools.zip -d "$ANDROID_HOME/cmdline-tools" \
    && mv "$ANDROID_HOME/cmdline-tools/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest" \
    && rm /tmp/cmdtools.zip \
    && yes | "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --licenses > /dev/null \
    && "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" "platforms;android-35" "build-tools;35.0.0" "platform-tools"
# Pre-warm the Gradle 8.11.1 wrapper distribution from the GitHub mirror:
# services.gradle.org is unreachable from the sandbox network, so the
# wrapper would otherwise fail before any build starts.
ENV GRADLE_USER_HOME=/root/.gradle
RUN mkdir -p "$GRADLE_USER_HOME/wrapper/dists/gradle-8.11.1-bin/bpt9gzteqjrbo1mjrsomdt32c" \
    && curl -sSL https://github.com/gradle/gradle-distributions/releases/download/v8.11.1/gradle-8.11.1-bin.zip -o /tmp/gradle.zip \
    && unzip -q /tmp/gradle.zip -d "$GRADLE_USER_HOME/wrapper/dists/gradle-8.11.1-bin/bpt9gzteqjrbo1mjrsomdt32c" \
    && touch "$GRADLE_USER_HOME/wrapper/dists/gradle-8.11.1-bin/bpt9gzteqjrbo1mjrsomdt32c/gradle-8.11.1-bin.zip.ok" \
    && rm /tmp/gradle.zip
ENV PATH=$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools
WORKDIR /workspace
