# syntax=docker/dockerfile:1.7
FROM debian:bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      g++ cmake ninja-build pkg-config \
      libgrpc++-dev protobuf-compiler protobuf-compiler-grpc \
      libprotobuf-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY services/pricing-engine/CMakeLists.txt .
COPY services/pricing-engine/src ./src
COPY services/pricing-engine/test ./test
COPY contracts/proto /proto

RUN cmake -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DPROTO_ROOT=/proto \
    && cmake --build build

# The rule engine's 55 assertions run at image build. Pricing is the one
# service where a wrong number is silently wrong rather than loudly broken.
FROM build AS test
RUN ./build/rules_test && ./build/rules_json_test

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
      libgrpc++1.51 libprotobuf32 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home souq

COPY --from=build /src/build/pricing-engine /usr/local/bin/pricing-engine
USER souq
EXPOSE 8089 9089
ENTRYPOINT ["/usr/local/bin/pricing-engine"]
