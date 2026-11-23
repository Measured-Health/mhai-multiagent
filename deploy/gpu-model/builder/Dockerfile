# The GGUF builder for swapping our own model (deploy/README.md, "Swapping our own model";
# gpu-model/swap-model.sh builds it on the GPU server and runs it twice):
#
#   1. `merge.py fetch`: downloads the allowed base (repo and pinned revision, safetensors,
#      JSON and tokenizer files only) into the host's cache. The only run with a network.
#   2. `merge.py build`: with no network, merges the researcher's QLoRA adapter into the
#      base (PEFT merge_and_unload, CPU, bfloat16), saves safetensors and the adapter's
#      tokenizer, converts to an f16 GGUF with llama.cpp's converter, quantises to Q4_K_M
#      and writes manifest.json (SHA-256, sizes, tool versions).
#
# Everything is pinned: the base image by digest, llama.cpp by commit (the commit the export
# step and the answer gate use, LLAMA_CPP_COMMIT in
# services/intelligence/intelligence/doceval/answers/llama_runtime.py), CPU torch and the
# Python packages by exact version (the set the first model was built with on the host,
# 22 Nov 2026). gguf-py is installed from the llama.cpp checkout on its own, never with its
# requirements file: that pins an older transformers which breaks the Gemma 4 tokenizer.
# No secret is ever passed to it; it never runs code from the adapter or the base
# (trust_remote_code is never set; safetensors only).
FROM python:3.12-slim-trixie@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

ARG LLAMA_CPP_COMMIT=b1b770d90d94cad9e9ea5b993bdbc383a522fb28
ENV LLAMA_CPP_COMMIT=${LLAMA_CPP_COMMIT} \
    LLAMA_CPP_DIR=/opt/llama.cpp \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HUB_DISABLE_TELEMETRY=1

RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN git init -q /opt/llama.cpp \
    && git -C /opt/llama.cpp fetch -q --depth 1 https://github.com/ggml-org/llama.cpp "$LLAMA_CPP_COMMIT" \
    && git -C /opt/llama.cpp checkout -q FETCH_HEAD \
    && test "$(git -C /opt/llama.cpp rev-parse HEAD)" = "$LLAMA_CPP_COMMIT" \
    && cmake -S /opt/llama.cpp -B /opt/llama.cpp/build -DLLAMA_CURL=OFF -DLLAMA_BUILD_IS_DEV=OFF \
      -DGGML_NATIVE=OFF -DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
      -DCMAKE_BUILD_TYPE=Release \
    && cmake --build /opt/llama.cpp/build --target llama-quantize -j "$(nproc)" \
    && rm -rf /opt/llama.cpp/.git

RUN pip install --index-url https://download.pytorch.org/whl/cpu torch==2.15.0 \
    && pip install transformers==5.24.0 peft==0.22.0 accelerate==1.16.0 safetensors==0.8.0 \
      huggingface-hub==2.4.0 tokenizers==0.23.3 numpy==2.5.5 sentencepiece==0.2.3 protobuf==7.36.2 \
    && pip install -e /opt/llama.cpp/gguf-py \
    && python -c "import transformers, peft, gguf; assert transformers.__version__ == '5.24.0'" \
    && pip freeze > /opt/pip-freeze.txt

COPY merge.py /opt/mhai/merge.py
RUN chmod -R a+rX /opt/llama.cpp /opt/mhai /opt/pip-freeze.txt
# The runs pass --user (the deploy user) and HOME=/tmp; never root.
USER 65534:65534
ENTRYPOINT ["python", "-I", "/opt/mhai/merge.py"]
