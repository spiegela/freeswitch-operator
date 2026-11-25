# The edge's FreeSWITCH image: the project's own FreeSWITCH 1.11.4 build (images/freeswitch-base) plus its speech
# detector (mod_speech_detect), cut down to what a SIP edge runs.
#
# The base builds FreeSWITCH from SignalWire's source with the modules an edge uses, mod_pgsql among them (sofia's
# track-calls recovery table and the core db in a shared PostgreSQL, so another node can `sofia recover` a lost
# node's calls), G.722, mod_lua and mod_opusfile. Its own Dockerfile says more.
#
# mod_speech_detect (mod_speech_detect/, Apache-2.0): barge-in's voice detectors, Silero VAD on ONNX Runtime and
# FreeSWITCH's own libfvad detector. ONNX Runtime comes from Microsoft's NuGet package (MIT; the C library for
# linux-x64 and linux-arm64, and its headers) and the model from Silero's repository (MIT), both checked by hash.
# It is compiled against the base's own headers and libfreeswitch, so it matches that build exactly.
#
# Cut down: the base keeps compilers, -dev packages, headers and debug symbols. The final image starts from the same
# Debian release and copies in FreeSWITCH, its modules and exactly the libraries those load (ldd's closure; never one
# the Debian base already has), stripped.
#
#   docker build -t freeswitch:dev images/freeswitch
#
# Before the base is published, build it first and point BASE at it:
#
#   docker build -t freeswitch-base:dev images/freeswitch-base
#   docker build --build-arg BASE=freeswitch-base:dev -t freeswitch:dev images/freeswitch

ARG BASE=ghcr.io/spiegela/freeswitch-base:1.11.4
# The Debian release the base is built on.
ARG RUNTIME=debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e

# Downloads, on the build machine's own platform: ONNX Runtime for both targets, and Silero's model.
FROM --platform=${BUILDPLATFORM} ${RUNTIME} AS fetch
ARG ONNXRUNTIME_VERSION=1.32.0
ARG ONNXRUNTIME_SHA256=a9150bc2992cbbb6722070da9910e1ffa989a8bfc67bdd5bb8e745ca755641db
ARG SILERO_VAD_VERSION=v6.3
ARG SILERO_VAD_MODEL_SHA256=f6f4407b41e05e277e7f5cfd9b9ae957b1dfe763c6076a934734c0b448e09b31
ARG SILERO_VAD_LICENSE_SHA256=2e63e9a38b6e8fc0c7bc37ce174caca1862870856c6daf5697cfb785e925520b
RUN apt-get update \
 && apt-get install -y --quiet --no-install-recommends ca-certificates curl unzip
WORKDIR /fetch
RUN curl -fsSL -o ort.nupkg \
      "https://api.nuget.org/v3-flatcontainer/microsoft.ml.onnxruntime/${ONNXRUNTIME_VERSION}/microsoft.ml.onnxruntime.${ONNXRUNTIME_VERSION}.nupkg" \
 && echo "${ONNXRUNTIME_SHA256}  ort.nupkg" | sha256sum -c - \
 && unzip -q ort.nupkg 'build/native/include/*' 'runtimes/linux-x64/native/libonnxruntime.so' \
      'runtimes/linux-arm64/native/libonnxruntime.so' LICENSE ThirdPartyNotices.txt \
 && mkdir -p onnxruntime/amd64 onnxruntime/arm64 \
 && mv build/native/include LICENSE ThirdPartyNotices.txt onnxruntime/ \
 && mv runtimes/linux-x64/native/libonnxruntime.so onnxruntime/amd64/ \
 && mv runtimes/linux-arm64/native/libonnxruntime.so onnxruntime/arm64/ \
 && rm -rf ort.nupkg build runtimes
RUN mkdir silero \
 && curl -fsSL -o silero/silero_vad.onnx \
      "https://raw.githubusercontent.com/snakers4/silero-vad/${SILERO_VAD_VERSION}/src/silero_vad/data/silero_vad.onnx" \
 && curl -fsSL -o silero/LICENSE "https://raw.githubusercontent.com/snakers4/silero-vad/${SILERO_VAD_VERSION}/LICENSE" \
 && printf '%s  %s\n' "${SILERO_VAD_MODEL_SHA256}" silero/silero_vad.onnx "${SILERO_VAD_LICENSE_SHA256}" silero/LICENSE \
      | sha256sum -c -

FROM ${BASE} AS build
ARG TARGETARCH
WORKDIR /src
COPY --from=fetch /fetch/onnxruntime/include /opt/onnxruntime/include
COPY --from=fetch /fetch/onnxruntime/${TARGETARCH}/libonnxruntime.so /opt/onnxruntime/lib/libonnxruntime.so
COPY mod_speech_detect/ mod_speech_detect/
# freeswitch.pc sits under FreeSWITCH's own prefix. The rpath finds libfreeswitch as the stock modules do;
# --no-undefined makes a symbol the base's libfreeswitch lacks fail here rather than when the module loads.
RUN export PKG_CONFIG_PATH=/usr/local/freeswitch/lib/pkgconfig \
 && gcc -shared -fPIC -O2 -std=gnu11 -Wall -Wextra -Wno-unused-parameter -Werror -o mod_speech_detect.so \
      mod_speech_detect/mod_speech_detect.c mod_speech_detect/silero.c \
      $(pkg-config --cflags freeswitch) -I/opt/onnxruntime/include \
      -Wl,--no-undefined -Wl,-rpath,/usr/local/freeswitch/lib $(pkg-config --libs freeswitch) \
      -L/opt/onnxruntime/lib -lonnxruntime

FROM ${RUNTIME} AS runtime

# Gathers what the final image keeps under /out, stripped.
FROM ${BASE} AS pick
ARG TARGETARCH
COPY --from=build /src/mod_speech_detect.so /usr/local/freeswitch/mod/
COPY --from=fetch /fetch/onnxruntime/${TARGETARCH}/libonnxruntime.so /usr/local/lib/libonnxruntime.so.1
COPY --from=fetch /fetch/silero/silero_vad.onnx /out/usr/local/share/speech_detect/silero_vad.onnx
COPY --from=fetch /fetch/silero/LICENSE /out/usr/local/share/doc/silero-vad/LICENSE
COPY --from=fetch /fetch/onnxruntime/LICENSE /fetch/onnxruntime/ThirdPartyNotices.txt /out/usr/local/share/doc/onnxruntime/
RUN --mount=type=bind,from=runtime,target=/rt <<'EOF'
set -eu
fs=/usr/local/freeswitch
ldconfig

cd "$fs/mod"
rm -f ./*.la
# The stock config's module list names modules the base doesn't build; a run on it (no config of your own) would
# log each one failing. Keep only the lines for modules that are here.
for m in $(sed -n -E 's/.*<load module="([^"]+)".*/\1/p' "$fs/conf/autoload_configs/modules.conf.xml"); do
  [ -f "$m.so" ] || sed -i "/<load module=\"$m\"/d" "$fs/conf/autoload_configs/modules.conf.xml"
done

# FreeSWITCH's tree, less what only building against it or video uses.
mkdir -p /out/usr/local
cp -a "$fs" /out/usr/local/
rm -rf /out$fs/include /out$fs/lib/pkgconfig /out$fs/lib/*.la /out$fs/lib/*.a /out$fs/fonts /out$fs/htdocs \
  /out$fs/images

# Every library the binaries and modules load (ldd lists the whole closure) that the Debian base lacks, by soname.
for f in "$fs"/bin/* "$fs"/lib/libfreeswitch.so.* "$fs"/mod/*.so; do
  [ -x "$f" ] && ldd "$f" 2>/dev/null | awk '$2 == "=>" && $3 ~ /^\// {print $1, $3}'
done | sort -u > /tmp/libs
cd /
while read -r soname lib; do
  case "$lib" in "$fs"/*) continue ;; esac
  [ -n "$(find /rt/usr/lib /rt/usr/local/lib -maxdepth 2 -name "$soname" 2>/dev/null)" ] && continue
  path=$(readlink -f "$(dirname "$lib")")/$(basename "$lib") file=$(readlink -f "$lib")
  cp -a --parents "$path" /out/
  [ "$file" = "$path" ] || cp -a --parents "$file" /out/
done < /tmp/libs

find /out -type f \( -name '*.so*' -o -perm -u+x \) -exec sh -c \
  'for f; do case "$(head -c4 "$f" | od -An -c | tr -d " ")" in "177ELF") strip --strip-unneeded "$f" ;; esac; done' sh {} +
EOF

FROM runtime
COPY --from=pick /out/ /
RUN ldconfig \
 && for f in /usr/local/freeswitch/bin/freeswitch /usr/local/freeswitch/mod/*.so; do \
      if ldd "$f" | grep 'not found'; then echo "$f: missing libraries" >&2; exit 1; fi; \
    done
# ORT_DISABLE_TELEMETRY: ONNX Runtime's Linux build carries Microsoft's telemetry client; this keeps it from starting
# (mod_speech_detect/silero.c says more).
ENV PATH=/usr/local/freeswitch/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    ORT_DISABLE_TELEMETRY=1
VOLUME ["/usr/local/freeswitch/log", "/usr/local/freeswitch/recordings", "/usr/local/freeswitch/sounds"]
# Runs on the image's stock config; the operator, kind and the bench mount their own and pass -conf.
CMD ["freeswitch", "-nonat", "-nf", "-nc"]
