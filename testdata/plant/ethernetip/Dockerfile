# The EtherNet/IP server the EtherNet/IP tests talk to: ab_server from
# libplctag 2.7.2, the simulator libplctag runs its own tests against. It
# answers as a ControlLogix over CIP, with the tags on the command line.
# tag_rw2 is libplctag's own command line client, in the image so a person can
# read a tag the way libplctag does and compare it with what Flow wrote.
# libplctag is MPL-2.0 or LGPL-2.0+; both run here as separate programs and
# nothing from them is linked into Flow.
FROM docker.io/library/debian:13-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN git clone -q --branch v2.7.2 https://github.com/libplctag/libplctag.git /src \
 && test "$(git -C /src rev-parse HEAD)" = 7712ed7f1ee05ba0ebfca641b3f5c43da52a66ed \
 && cmake -S /src -B /build -DBUILD_EXAMPLES=0 -DBUILD_TESTS=0 -DUSE_MEM_SANITIZERS=0 -DCMAKE_BUILD_TYPE=Release \
 && cmake --build /build --target ab_server --target tag_rw2 -j4 \
 && cp "$(find /build -name ab_server -type f | head -1)" /ab_server \
 && cp "$(find /build -name tag_rw2 -type f | head -1)" /tag_rw2

FROM docker.io/library/debian:13-slim
COPY --from=build /ab_server /usr/local/bin/ab_server
COPY --from=build /tag_rw2 /usr/local/bin/tag_rw2
EXPOSE 44818
ENTRYPOINT ["/usr/local/bin/ab_server"]
