# docsandbox (ADR 0016; documents design §5 step 4, §14 S1-S2): renders one
# uploaded file per fresh nsjail process with pdfium (pypdfium2, BSD/Apache)
# and Pillow. It holds no credential and no key; compose gives it no network
# (network_mode none) and only the jobs tmpfs it shares with the documents
# service.
#   docker build -t mhai-docsandbox:local services/docsandbox
#   docker build --target test -t mhai-docsandbox:test services/docsandbox   (the jailed tests)
# Every base is pinned by digest, nsjail by commit, the Python packages by
# hash (requirements.txt, from requirements.in).

# nsjail 3.6, built from its tagged commit.
FROM python:3.12-slim-trixie@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b AS nsjail
ARG NSJAIL_COMMIT=f78475530b46d0186111a9096b30725f816b55fe
RUN apt-get update && apt-get install -y --no-install-recommends \
      autoconf bison flex gcc g++ git libprotobuf-dev libnl-route-3-dev libtool make pkg-config \
      protobuf-compiler ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --filter=blob:none https://github.com/google/nsjail.git /src/nsjail \
    && cd /src/nsjail && git checkout --detach "$NSJAIL_COMMIT" \
    && test "$(git rev-parse HEAD)" = "$NSJAIL_COMMIT" \
    && make -j"$(nproc)" && install -m 0755 nsjail /out-nsjail

FROM python:3.12-slim-trixie@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b AS runtime
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
RUN apt-get update && apt-get install -y --no-install-recommends libprotobuf32t64 libnl-route-3-200 \
    && rm -rf /var/lib/apt/lists/*
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes --only-binary=:all: -r /tmp/requirements.txt && rm /tmp/requirements.txt
COPY --from=nsjail /out-nsjail /usr/local/bin/nsjail
# The renderer at a fixed path (bound read-only into each jail), the server
# beside it.
COPY docsandbox/ /opt/docsandbox/docsandbox/
RUN cp /opt/docsandbox/docsandbox/render.py /opt/docsandbox/render.py && chmod -R a-w /opt/docsandbox
WORKDIR /opt/docsandbox
ENV PYTHONPATH=/opt/docsandbox

# The jailed tests (test_jail.py) and the renderer's; never shipped.
FROM runtime AS test
COPY tests/ /opt/docsandbox/tests/
ENV DOCSANDBOX_JAIL_TESTS=1 SYNTHETIC_CREDENTIAL=test-only-not-a-secret
RUN mkdir -p /jobs && chmod 0700 /jobs
CMD ["python3", "-m", "unittest", "-v", "tests.test_jail", "tests.test_render", "tests.test_server"]

# The image compose runs: as root inside the container (nsjail needs
# CAP_SYS_ADMIN to build each jail; the renderer runs as uid 65532 with no
# capability), no network, a read-only root.
FROM runtime
CMD ["python3", "-m", "docsandbox.server", "/jobs"]
