# SPDX-License-Identifier:Apache-2.0
FROM quay.io/frrouting/frr@sha256:89e5e1ec8b17c5dffaf57dbac58a29ebeb3731a84e5bc09dad1662adff2ba7f5 AS build

RUN apk add --no-cache build-base autoconf automake libtool bison flex \
    json-c-dev c-ares-dev readline-dev libcap-dev rtrlib-dev lua5.3-dev \
    elfutils-dev protobuf-c-dev protobuf-c-compiler python3-dev py3-sphinx \
    py3-pytest texinfo
WORKDIR /source
COPY source/ ./
COPY source-revision.txt /evidence/source-revision.txt
COPY source-archive.sha256 /evidence/source-archive.sha256
ENV CFLAGS="-Os -fno-omit-frame-pointer -funwind-tables -fstack-clash-protection -fno-plt" \
    CXXFLAGS="-Os -fno-omit-frame-pointer -funwind-tables -fstack-clash-protection -fno-plt" \
    CPPFLAGS="-D_FORTIFY_SOURCE=3" \
    LDFLAGS="-Wl,--as-needed,-O1,--sort-common"
RUN ./bootstrap.sh && \
    ./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var \
      --sbindir=/usr/lib/frr --libdir=/usr/lib --enable-rpki --enable-vtysh \
      --enable-multipath=64 --enable-vty-group=frrvty --enable-user=frr \
      --enable-group=frr --enable-pcre2posix --enable-scripting \
      --with-pkg-extra-version=_git CC=cc CXX=c++ && \
    make -j4 all tests/bgpd/test_gr_replay tests/bgpd/test_gr_eor tests/bgpd/test_peer_attr && \
    (cd tests && PYTHONPATH=helpers/python python3 -m pytest -q \
      --junitxml=/evidence/native.xml \
      bgpd/test_gr_replay.py bgpd/test_gr_eor.py bgpd/test_peer_attr.py) && \
    make DESTDIR=/stage install && \
    cp config.h config.log config.status Makefile /evidence/ && \
    apk info -v > /evidence/build-packages.txt && \
    find /stage -type f -print0 | sort -z | xargs -0 sha256sum > /evidence/payload.sha256

FROM scratch AS evidence
COPY --from=build /evidence/ /

FROM quay.io/frrouting/frr@sha256:89e5e1ec8b17c5dffaf57dbac58a29ebeb3731a84e5bc09dad1662adff2ba7f5 AS runtime
LABEL org.opencontainers.image.revision="9cfa2040d418cbfb11aceb2d07443c8f6289dc0f"
COPY --from=build /stage/ /
