# syntax=docker/dockerfile:1.7
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#
#     docker build -f services/catalog-service/Dockerfile .
#
# Uniform across all thirteen images. Ten of them genuinely need it — a shared
# library under libs/ or contracts/ that a build context cannot reach above
# itself to find — and the remaining three follow suit so there is one rule
# rather than a per-image exception somebody has to remember.
FROM eclipse-temurin:21-jdk-alpine AS build
WORKDIR /src
COPY services/catalog-service/pom.xml .
# Dependencies first so a code-only change reuses this layer.
RUN --mount=type=cache,target=/root/.m2 \
    apk add --no-cache maven && mvn -B -q dependency:go-offline
COPY services/catalog-service/src ./src
RUN --mount=type=cache,target=/root/.m2 mvn -B -q package -DskipTests
RUN java -Djarmode=layertools -jar target/catalog-service-1.0.0.jar extract --destination /layers

FROM eclipse-temurin:21-jre-alpine AS runtime
RUN addgroup -S souq && adduser -S -G souq souq
WORKDIR /app
# Layered extraction: dependencies change rarely, application classes change
# every build. Splitting them keeps image pushes small.
COPY --from=build /layers/dependencies/ ./
COPY --from=build /layers/spring-boot-loader/ ./
COPY --from=build /layers/snapshot-dependencies/ ./
COPY --from=build /layers/application/ ./
USER souq
EXPOSE 8082
# Container-aware heap sizing. Without MaxRAMPercentage the JVM sizes itself
# against the node, not the cgroup, and gets OOM-killed on a small limit.
ENV JAVA_TOOL_OPTIONS="-XX:MaxRAMPercentage=70 -XX:+UseG1GC -XX:+ExitOnOutOfMemoryError"
ENTRYPOINT ["java","org.springframework.boot.loader.launch.JarLauncher"]
