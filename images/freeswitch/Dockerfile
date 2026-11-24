# The edge's FreeSWITCH image: drachtio's build of FreeSWITCH 1.10.10, plus mod_pgsql, which drachtio's build leaves
# out, and the project's own speech detector (mod_speech_detect), cut down to what a SIP edge runs.
#
# mod_pgsql: with it, sofia's track-calls recovery table and the core db can live in a shared PostgreSQL (Aurora)
# instead of each pod's own SQLite, so another node can `sofia recover` a lost node's calls. It is one C file,
# compiled against the headers and libfreeswitch the base image installed itself, so it matches that build exactly
# (drachtio patches FreeSWITCH's .c files, never its headers). The source is FreeSWITCH's at the base's own commit
# (4cb05e7, tag v1.10.10; `freeswitch -version` names it), checked by hash.
#
# mod_speech_detect (mod_speech_detect/, Apache-2.0): barge-in's voice detectors, Silero VAD on ONNX Runtime and
# FreeSWITCH's own libfvad detector. ONNX Runtime comes from Microsoft's NuGet package (MIT; the C library for
# linux-x64 and linux-arm64, and its headers) and the model from Silero's repository (MIT), both checked by hash.
#
# Cut down: drachtio's image is about 1.5 GB, most of it jambonz's modules (cloud speech recognisers, synthesisers,
# speech-to-speech agents, and voice detectors under jambonz's AGPL-or-jambonz-only licence) and the SDKs they pull
# in; and libfreeswitch and the gRPC libraries it links, shipped with their debug symbols. The final image starts
# from the same Debian release and copies in FreeSWITCH, the modules it keeps (every one but DROP_MODULES) and
# exactly the libraries those load (ldd's closure; never one the Debian base already has), stripped.
#
#   docker build -t freeswitch:dev images/freeswitch

ARG BASE=drachtio/drachtio-freeswitch-mrf:0.9.6@sha256:cb81acc1112990ca33693374f1de7443d44cf9c95e6ae6c02ba1edcee4b68bba
# The Debian release drachtio's image is built on (bookworm; /etc/debian_version there is 12.13).
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
ARG FREESWITCH_COMMIT=4cb05e7f4a
ARG MOD_PGSQL_SHA256=8da777da6feda4f672864a398879baf0a090e8e33ada66076ff2ef6307fb685d
RUN apt-get update \
 && apt-get install -y --quiet --no-install-recommends ca-certificates curl gcc libc6-dev libpq-dev pkg-config
WORKDIR /src
RUN curl -fsSL -o mod_pgsql.c \
      "https://raw.githubusercontent.com/signalwire/freeswitch/${FREESWITCH_COMMIT}/src/mod/databases/mod_pgsql/mod_pgsql.c" \
 && echo "${MOD_PGSQL_SHA256}  mod_pgsql.c" | sha256sum -c -
COPY --from=fetch /fetch/onnxruntime/include /opt/onnxruntime/include
COPY --from=fetch /fetch/onnxruntime/${TARGETARCH}/libonnxruntime.so /opt/onnxruntime/lib/libonnxruntime.so
COPY mod_speech_detect/ mod_speech_detect/
# freeswitch.pc sits under FreeSWITCH's own prefix. The rpath finds libfreeswitch as the stock modules do;
# --no-undefined makes a symbol the base's libfreeswitch lacks fail here rather than when the module loads.
RUN export PKG_CONFIG_PATH=/usr/local/freeswitch/lib/pkgconfig \
 && gcc -shared -fPIC -O2 -Wall -o mod_pgsql.so mod_pgsql.c \
      $(pkg-config --cflags freeswitch libpq) \
      -Wl,--no-undefined -Wl,-rpath,/usr/local/freeswitch/lib $(pkg-config --libs freeswitch libpq) \
 && gcc -shared -fPIC -O2 -std=gnu11 -Wall -Wextra -Wno-unused-parameter -Werror -o mod_speech_detect.so \
      mod_speech_detect/mod_speech_detect.c mod_speech_detect/silero.c \
      $(pkg-config --cflags freeswitch) -I/opt/onnxruntime/include \
      -Wl,--no-undefined -Wl,-rpath,/usr/local/freeswitch/lib $(pkg-config --libs freeswitch) \
      -L/opt/onnxruntime/lib -lonnxruntime

FROM ${RUNTIME} AS runtime

# Gathers what the final image keeps under /out, stripped.
FROM ${BASE} AS pick
ARG TARGETARCH
# jambonz's modules: the cloud speech ones (recognisers, synthesisers, speech-to-speech agents, Lex, Dialogflow,
# dubbing, audio streaming) and the voice detectors mod_speech_detect replaces; and mod_opusfile, which cannot load
# even in drachtio's image (no libopusfile there). A regular expression over the module file names.
ARG DROP_MODULES='_(transcribe|transcribe_ws|tts|tts_streaming|s2s)\.so$|^mod_(aws_lex|dialogflow|dub|audio_fork|opusfile|vad_silero|vad_detect)\.so$'
RUN apt-get update \
 && apt-get install -y --quiet --no-install-recommends binutils libpq5
COPY --from=build /src/mod_pgsql.so /src/mod_speech_detect.so /usr/local/freeswitch/mod/
# Our ONNX Runtime in place of drachtio's (which only jambonz's detector used).
COPY --from=fetch /fetch/onnxruntime/${TARGETARCH}/libonnxruntime.so /tmp/libonnxruntime.so
COPY --from=fetch /fetch/silero/silero_vad.onnx /out/usr/local/share/speech_detect/silero_vad.onnx
COPY --from=fetch /fetch/silero/LICENSE /out/usr/local/share/doc/silero-vad/LICENSE
COPY --from=fetch /fetch/onnxruntime/LICENSE /fetch/onnxruntime/ThirdPartyNotices.txt /out/usr/local/share/doc/onnxruntime/
RUN --mount=type=bind,from=runtime,target=/rt <<'EOF'
set -eu
fs=/usr/local/freeswitch
rm -rf /usr/local/lib/libonnxruntime* /usr/local/share/silero_vad
mv /tmp/libonnxruntime.so /usr/local/lib/libonnxruntime.so.1
ldconfig

cd "$fs/mod"
ls | grep -E "$DROP_MODULES" | xargs rm -f
rm -f ./*.la
# The stock config's module list loads the dropped ones; a run on it (no config of your own) would log each failing.
sed -i -E '/<load module="([^"]+)"/{h;s/.*<load module="([^"]+)".*/\1.so/;/'"$DROP_MODULES"'/{g;d};g}' \
  "$fs/conf/autoload_configs/modules.conf.xml"

# FreeSWITCH's tree, less what only building against it or video uses. (cp --parents wants to run from /.)
cd /
mkdir -p /out/usr/local
cp -a "$fs" /out/usr/local/
rm -rf /out$fs/include /out$fs/lib/pkgconfig /out$fs/lib/*.la /out$fs/fonts /out$fs/htdocs /out$fs/images
cp -a --parents /entrypoint.sh /out/

# Every library the binaries and modules load (ldd lists the whole closure) that the Debian base lacks, by soname:
# drachtio's image keeps copies of some Debian libraries in /usr/lib itself, and the base's own must win.
for f in "$fs"/bin/* "$fs"/lib/libfreeswitch.so.* "$fs"/mod/*.so; do
  [ -x "$f" ] && ldd "$f" 2>/dev/null | awk '$2 == "=>" && $3 ~ /^\// {print $1, $3}'
done | sort -u > /tmp/libs
while read -r soname lib; do
  case "$lib" in "$fs"/*) continue ;; esac
  [ -n "$(find /rt/usr/lib /rt/usr/local/lib -maxdepth 2 -name "$soname" 2>/dev/null)" ] && continue
  path=$(readlink -f "$(dirname "$lib")")/$(basename "$lib") file=$(readlink -f "$lib")
  cp -a --parents "$path" /out/
  [ "$file" = "$path" ] || cp -a --parents "$file" /out/
done < /tmp/libs

# Debug symbols are most of libfreeswitch's and gRPC's size.
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
    LD_LIBRARY_PATH=/usr/local/lib \
    ORT_DISABLE_TELEMETRY=1
VOLUME ["/usr/local/freeswitch/log", "/usr/local/freeswitch/recordings", "/usr/local/freeswitch/sounds"]
ENTRYPOINT ["/entrypoint.sh"]
CMD ["freeswitch"]
