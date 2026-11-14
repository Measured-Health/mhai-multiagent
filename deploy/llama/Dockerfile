# The llama sidecar (profile ownmodel; own answer model plan, PR 3): llama.cpp's
# llama-server built at the pinned commit (the commit the export step converts with and
# the answer gate scores with: LLAMA_CPP_COMMIT in
# services/intelligence/intelligence/doceval/answers/llama_runtime.py), without curl, so
# it cannot fetch a model, plus entrypoint.sh, which re-hashes the GGUF against the
# worker's active.json before starting it. Builds for linux/arm64 (the demo host) and
# linux/amd64 alike: GGML_NATIVE=OFF, no CPU-specific flags.
#
#   docker build -t mhai-llama:local deploy/llama
#
# With an NVIDIA GPU (the intelligence host, docs/deploy/remote-intelligence.md, D9):
# the same commit built with CUDA on NVIDIA's CUDA images; the entrypoint offloads
# LLAMA_GPU_LAYERS layers. deploy/intelligence-host/compose.gpu.yml passes:
#
#   docker build -t mhai-llama:cuda --build-arg GGML_CUDA=ON \
#     --build-arg BUILD_BASE=nvidia/cuda:12.6.3-devel-ubuntu24.04 \
#     --build-arg RUN_BASE=nvidia/cuda:12.6.3-runtime-ubuntu24.04 deploy/llama
ARG DEBIAN=debian:bookworm-20251020-slim@sha256:78d2f66e0fec9e5a39fb2c72ea5e052b548df75602b5215ed01a17171529f706
ARG BUILD_BASE=${DEBIAN}
ARG RUN_BASE=${DEBIAN}

FROM ${BUILD_BASE} AS build
ARG LLAMA_CPP_COMMIT=b1b770d90d94cad9e9ea5b993bdbc383a522fb28
# OFF (the default): CPU only. ON: CUDA, for the GPUs of T4, A10G/RTX 30, L4/RTX 40
# and A100 (compute 7.5, 8.6, 8.9, 8.0). libcuda comes from the host's driver at run
# time, so the CUDA build links against the toolkit's stub and can't run --version.
ARG GGML_CUDA=OFF
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git init -q /src \
    && git -C /src fetch -q --depth 1 https://github.com/ggml-org/llama.cpp "$LLAMA_CPP_COMMIT" \
    && git -C /src checkout -q FETCH_HEAD \
    && test "$(git -C /src rev-parse HEAD)" = "$LLAMA_CPP_COMMIT"
RUN case "$GGML_CUDA" in \
      OFF) cuda="" ;; \
      ON) cuda="-DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=75;80;86;89 -DCMAKE_EXE_LINKER_FLAGS=-Wl,--allow-shlib-undefined" ;; \
      *) echo "GGML_CUDA must be ON or OFF" >&2; exit 1 ;; \
    esac \
    && cmake -S /src -B /src/build -DLLAMA_CURL=OFF -DLLAMA_BUILD_IS_DEV=OFF -DGGML_NATIVE=OFF \
      -DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
      -DCMAKE_BUILD_TYPE=Release $cuda \
    && cmake --build /src/build --target llama-server -j "$(nproc)" \
    && if [ "$GGML_CUDA" = OFF ]; then /src/build/bin/llama-server --version; fi

FROM ${RUN_BASE}
# libgomp for ggml's threads; coreutils' sha256sum is in the base image. The same uid as
# the pipeline image's appuser (1000), which owns the verified cache it reads.
RUN apt-get update && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/* \
    && { getent group 1000 >/dev/null || groupadd --gid 1000 llama; } \
    && { getent passwd 1000 >/dev/null || useradd --uid 1000 --gid 1000 --no-create-home --shell /usr/sbin/nologin llama; }
COPY --from=build /src/build/bin/llama-server /usr/local/bin/llama-server
COPY entrypoint.sh /usr/local/bin/llama-entrypoint
RUN chmod 0555 /usr/local/bin/llama-entrypoint /usr/local/bin/llama-server
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/llama-entrypoint"]
