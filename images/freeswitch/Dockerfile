# The edge's FreeSWITCH image: drachtio's image (FreeSWITCH 1.10.10 with jambonz's modules, the barge-in VAD ones
# among them) plus mod_pgsql, which drachtio's build leaves out. With mod_pgsql, sofia's track-calls recovery table
# and the core db can live in a shared PostgreSQL (Aurora) instead of each pod's own SQLite, so another node can
# `sofia recover` a lost node's calls.
#
# mod_pgsql is one C file. It is compiled against the headers and libfreeswitch the base image installed itself, so
# it matches that build exactly (drachtio patches FreeSWITCH's .c files, never its headers). The source is
# FreeSWITCH's at the base's own commit (4cb05e7, tag v1.10.10; `freeswitch -version` names it), checked by hash.
#
#   docker build -t freeswitch:dev images/freeswitch

ARG BASE=drachtio/drachtio-freeswitch-mrf:0.9.6@sha256:cb81acc1112990ca33693374f1de7443d44cf9c95e6ae6c02ba1edcee4b68bba

FROM ${BASE} AS build
ARG FREESWITCH_COMMIT=4cb05e7f4a
ARG MOD_PGSQL_SHA256=8da777da6feda4f672864a398879baf0a090e8e33ada66076ff2ef6307fb685d
RUN apt-get update \
 && apt-get install -y --quiet --no-install-recommends ca-certificates curl gcc libc6-dev libpq-dev pkg-config
WORKDIR /src
RUN curl -fsSL -o mod_pgsql.c \
      "https://raw.githubusercontent.com/signalwire/freeswitch/${FREESWITCH_COMMIT}/src/mod/databases/mod_pgsql/mod_pgsql.c" \
 && echo "${MOD_PGSQL_SHA256}  mod_pgsql.c" | sha256sum -c -
# freeswitch.pc sits under FreeSWITCH's own prefix. The rpath finds libfreeswitch as the stock modules do;
# --no-undefined makes a symbol the base's libfreeswitch lacks fail here rather than when the module loads.
RUN export PKG_CONFIG_PATH=/usr/local/freeswitch/lib/pkgconfig \
 && gcc -shared -fPIC -O2 -Wall -o mod_pgsql.so mod_pgsql.c \
      $(pkg-config --cflags freeswitch libpq) \
      -Wl,--no-undefined -Wl,-rpath,/usr/local/freeswitch/lib $(pkg-config --libs freeswitch libpq)

FROM ${BASE}
RUN apt-get update \
 && apt-get install -y --quiet --no-install-recommends libpq5 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/mod_pgsql.so /usr/local/freeswitch/mod/mod_pgsql.so
