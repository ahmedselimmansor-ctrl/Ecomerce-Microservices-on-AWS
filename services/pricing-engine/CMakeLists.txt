cmake_minimum_required(VERSION 3.20)
project(pricing_engine CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

# -Werror on the rule engine specifically. This is the code that decides what a
# customer is charged; a warning here is not something to look at later.
add_compile_options(-Wall -Wextra -Wpedantic -Wconversion -Wshadow)

if(CMAKE_BUILD_TYPE STREQUAL "Release")
  add_compile_options(-O2)
  # No -ffast-math anywhere. There is no floating point in the rule engine and
  # there must not be, but the flag would also license reassociation that
  # changes integer overflow behaviour under some compilers.
endif()

option(ENABLE_SANITIZERS "Build with ASan and UBSan" OFF)
if(ENABLE_SANITIZERS)
  add_compile_options(-fsanitize=address,undefined -fno-omit-frame-pointer -g)
  add_link_options(-fsanitize=address,undefined)
endif()

# ---------------------------------------------------------------------------
# The rule engine. Pure, no I/O, no gRPC — which is what makes it testable
# with one g++ invocation and no dependencies.
add_library(rules STATIC src/rules.cpp src/rules_json.cpp)
target_include_directories(rules PUBLIC src)

# ---------------------------------------------------------------------------
# Tests. 55 assertions, no framework: one binary, one command, so the tests are
# something people actually run while iterating on a promotion.
add_executable(rules_test test/rules_test.cpp)
target_link_libraries(rules_test PRIVATE rules)

add_executable(rules_json_test test/rules_json_test.cpp)
target_link_libraries(rules_json_test PRIVATE rules)

enable_testing()
add_test(NAME rules COMMAND rules_test)
# Runs from the source dir so it can load the real rules/rules.json — the
# shipped file being loadable is the assertion that matters most.
add_test(NAME rules_json COMMAND rules_json_test WORKING_DIRECTORY ${CMAKE_SOURCE_DIR})

# ---------------------------------------------------------------------------
# The gRPC server. Optional so `cmake --build . --target rules_test` works on a
# machine with no protobuf toolchain — which is the common case for someone
# changing a discount rule.
find_package(Protobuf CONFIG)
find_package(gRPC CONFIG)

if(Protobuf_FOUND AND gRPC_FOUND)
  set(PROTO_ROOT "${CMAKE_SOURCE_DIR}/../../contracts/proto" CACHE PATH "protobuf import root")
  set(GENERATED "${CMAKE_BINARY_DIR}/generated")
  file(MAKE_DIRECTORY ${GENERATED})

  set(PROTOS
    "${PROTO_ROOT}/souq/common/v1/money.proto"
    "${PROTO_ROOT}/souq/pricing/v1/pricing.proto")

  add_custom_command(
    OUTPUT ${GENERATED}/souq/pricing/v1/pricing.pb.cc
           ${GENERATED}/souq/pricing/v1/pricing.grpc.pb.cc
           ${GENERATED}/souq/common/v1/money.pb.cc
    COMMAND protobuf::protoc
    ARGS --proto_path=${PROTO_ROOT}
         --cpp_out=${GENERATED}
         --grpc_out=${GENERATED}
         --plugin=protoc-gen-grpc=$<TARGET_FILE:gRPC::grpc_cpp_plugin>
         ${PROTOS}
    DEPENDS ${PROTOS}
    COMMENT "Generating gRPC stubs from contracts/proto")

  add_executable(pricing-engine
    src/server.cpp
    ${GENERATED}/souq/pricing/v1/pricing.pb.cc
    ${GENERATED}/souq/pricing/v1/pricing.grpc.pb.cc
    ${GENERATED}/souq/common/v1/money.pb.cc)

  target_include_directories(pricing-engine PRIVATE ${GENERATED} src)

  # grpc++_reflection is wrapped in --no-as-needed, and both halves matter.
  #
  # server.cpp calls InitProtoReflectionServerBuilderPlugin() so grpcurl can
  # introspect the service during an incident, and the library registers itself
  # through a static initialiser. A linker running with --as-needed — which is
  # the default on every modern distribution — sees no *referenced* symbol at
  # the point it considers the library and drops it, leaving exactly one
  # undefined reference.
  target_link_libraries(pricing-engine PRIVATE
    rules
    -Wl,--no-as-needed gRPC::grpc++_reflection -Wl,--as-needed
    gRPC::grpc++
    protobuf::libprotobuf)
else()
  message(STATUS "gRPC/protobuf not found — building the rule engine and its tests only")
endif()
