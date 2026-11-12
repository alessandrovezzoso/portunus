# whisper.cpp's HTTP server with the Whisper large-v3-turbo model, for
# internal/stt/openaicompat. deploy/compose.yaml builds it: make stt-up.
#
# It is built here rather than pulled. The upstream image has a different tag
# per architecture and ships the whole build toolchain. Building on the host
# that runs it also lets the compiler use every instruction that CPU has
# (GGML_NATIVE). The flip side: an image built on one machine may not run on
# another, so build it where it runs and never push it.

# Both pins are deliberate. Upgrade them together with a listening test,
# because a new release or model can change what gets transcribed.
ARG WHISPER_CPP_VERSION=v1.9.5
ARG MODEL_FILE=ggml-large-v3-turbo-q5_0.bin
ARG MODEL_SHA256=394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2
# A Hugging Face commit of ggerganov/whisper.cpp, so the URL cannot start
# serving different bytes. The checksum above is the actual guarantee.
ARG MODEL_REVISION=5359861c739e955e79d9a303bcbc70fb988958b1

FROM debian:13 AS build
ARG WHISPER_CPP_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${WHISPER_CPP_VERSION}" https://github.com/ggml-org/whisper.cpp /src
WORKDIR /src
# Static libraries, so the runtime image needs only the C++ and OpenMP
# runtimes. Only the server is built: the other examples and the tests are
# not needed here.
RUN cmake -B build \
      -DCMAKE_BUILD_TYPE=Release \
      -DBUILD_SHARED_LIBS=OFF \
      -DGGML_NATIVE=ON \
      -DWHISPER_BUILD_TESTS=OFF \
      -DWHISPER_BUILD_EXAMPLES=ON \
      -DWHISPER_BUILD_SERVER=ON \
 && cmake --build build --target whisper-server -j "$(nproc)"

# The model has its own stage, so changing the compiler flags does not
# download 574 MB again, and the reverse.
FROM debian:13-slim AS model
ARG MODEL_FILE
ARG MODEL_SHA256
ARG MODEL_REVISION
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN mkdir /models \
 && curl -fL --retry 3 -o "/models/${MODEL_FILE}" \
      "https://huggingface.co/ggerganov/whisper.cpp/resolve/${MODEL_REVISION}/${MODEL_FILE}" \
 && echo "${MODEL_SHA256}  /models/${MODEL_FILE}" | sha256sum -c -

FROM debian:13-slim
ARG MODEL_FILE
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/build/bin/whisper-server /usr/local/bin/whisper-server
COPY --from=model /models /models
# The server runs as an unprivileged user: it only reads the model and
# answers HTTP on the port compose publishes on 127.0.0.1.
RUN useradd --system --no-create-home whisper
USER whisper
ENV MODEL_PATH=/models/${MODEL_FILE}
EXPOSE 8080
# compose.yaml passes the server flags as the command.
ENTRYPOINT ["sh", "-c", "exec whisper-server -m \"$MODEL_PATH\" \"$@\"", "whisper-server"]
