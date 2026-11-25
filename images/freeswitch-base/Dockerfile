# The project's FreeSWITCH base image: FreeSWITCH 1.11.4 built from SignalWire's source on Debian bookworm, with
# the libraries it wants newer than Debian ships (sofia-sip, spandsp, libfvad) built from theirs. It takes the place
# of drachtio's image (drachtio/drachtio-freeswitch-mrf, FreeSWITCH 1.10.10) as what images/freeswitch builds on.
#
# It is a build base, not what a node runs: it keeps the compilers, every -dev package, and FreeSWITCH's headers and
# pkg-config file, so images/freeswitch can compile its own modules against exactly this build and then copy out
# only what an edge loads. Every source is fetched at a tag and checked against the commit that tag named.
#
# The modules (MODULES below) are the ones an edge or the bench loads, and three more that drachtio's build lacked
# or could not load:
# - G.722 (mod_spandsp's codecs, on spandsp's G.722), for the codec-g722 bench scenario and wideband trunks.
# - mod_lua. The edge's own config doesn't load it: with it loaded, a dialplan or API caller can run inline Lua.
# - mod_opusfile, Ogg Opus files, on Debian's libopusfile and libopusenc.
# Video is left out (--disable-libvpx --disable-libyuv): a SIP edge relays audio.
#
#   docker build -t freeswitch-base:dev images/freeswitch-base

ARG DEBIAN=debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e

FROM ${DEBIAN}
ARG FREESWITCH_VERSION=v1.11.4
ARG FREESWITCH_COMMIT=795228ad43ecc8ebd679d15fc189e66de94c252a
ARG SOFIA_SIP_VERSION=v1.13.19
ARG SOFIA_SIP_COMMIT=6dce59d35535e6dd3945c7b0f76dbf1ba884202d
ARG SPANDSP_VERSION=v3.1.1
ARG SPANDSP_COMMIT=8f1e1646bdec99eac5fd2cd92c35563f736b9b89
ARG LIBFVAD_VERSION=v1.0
ARG LIBFVAD_COMMIT=99fd86bbb8093a8d0433db3da1d8959c1682adcc
# FreeSWITCH's modules.conf, one source directory per line.
ARG MODULES="\
applications/mod_commands applications/mod_db applications/mod_dptools applications/mod_expr applications/mod_hash \
applications/mod_spandsp \
codecs/mod_amr codecs/mod_g723_1 codecs/mod_opus \
databases/mod_pgsql \
dialplans/mod_dialplan_xml \
endpoints/mod_loopback endpoints/mod_sofia \
event_handlers/mod_event_socket \
formats/mod_local_stream formats/mod_native_file formats/mod_opusfile formats/mod_sndfile formats/mod_tone_stream \
languages/mod_lua \
loggers/mod_console loggers/mod_logfile loggers/mod_syslog \
say/mod_say_en"

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --quiet --no-install-recommends \
      autoconf automake ca-certificates g++ gcc git libtool libtool-bin make pkg-config \
      libcurl4-openssl-dev libedit-dev libjpeg62-turbo-dev libldns-dev liblua5.3-dev libminiupnpc-dev \
      libnatpmp-dev libopus-dev libopusenc-dev libopusfile-dev libpcre2-dev libpq-dev libsndfile1-dev \
      libspeex-dev libspeexdsp-dev libsqlite3-dev libssl-dev libtiff-dev uuid-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# A shallow clone at the tag, then the tag must still name the commit pinned here.
COPY <<'EOF' /usr/local/bin/fetch
#!/bin/sh
set -eu
git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$2" "$1" "$4"
got=$(git -C "$4" rev-parse HEAD)
[ "$got" = "$3" ] || { echo "$1 $2 is $got, not $3" >&2; exit 1; }
EOF
RUN chmod +x /usr/local/bin/fetch

# The libraries, under /usr/local (images/freeswitch copies out whatever there the modules load).
RUN fetch https://github.com/dpirch/libfvad.git "$LIBFVAD_VERSION" "$LIBFVAD_COMMIT" libfvad \
 && cd libfvad && autoreconf -i >/dev/null 2>&1 && ./configure --quiet --prefix=/usr/local \
 && make -j"$(nproc)" >/dev/null && make install >/dev/null
RUN fetch https://github.com/freeswitch/sofia-sip.git "$SOFIA_SIP_VERSION" "$SOFIA_SIP_COMMIT" sofia-sip \
 && cd sofia-sip && ./bootstrap.sh >/dev/null 2>&1 \
 && ./configure --quiet --prefix=/usr/local --with-pic --with-glib=no --without-doxygen --disable-stun \
 && make -j"$(nproc)" >/dev/null && make install >/dev/null
RUN fetch https://github.com/freeswitch/spandsp.git "$SPANDSP_VERSION" "$SPANDSP_COMMIT" spandsp \
 && cd spandsp && ./bootstrap.sh >/dev/null 2>&1 && ./configure --quiet --prefix=/usr/local --with-pic \
 && make -j"$(nproc)" >/dev/null && make install >/dev/null \
 && ldconfig

# FreeSWITCH, under its own prefix (/usr/local/freeswitch), as drachtio's was.
RUN fetch https://github.com/signalwire/freeswitch.git "$FREESWITCH_VERSION" "$FREESWITCH_COMMIT" freeswitch \
 && cd freeswitch \
 && printf '%s\n' $MODULES > modules.conf \
 && ./bootstrap.sh -j >/dev/null 2>&1 \
 && ./configure --quiet --disable-debug --disable-libvpx --disable-libyuv \
 && make -j"$(nproc)" >/dev/null \
 && make install >/dev/null \
 && for m in $MODULES; do \
      [ -f "/usr/local/freeswitch/mod/${m#*/}.so" ] || { echo "${m#*/} was not built" >&2; exit 1; }; \
    done \
 && cd / && rm -rf /src && ldconfig

ENV PATH=/usr/local/freeswitch/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
CMD ["freeswitch", "-nonat", "-nf", "-nc"]
