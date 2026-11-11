# The emr service and egress-emr, its CONNECT proxy (ADR 0014; one image,
# two binaries: compose runs the proxy with entrypoint /egress-emr). Built
# from services/ (it requires services/gokit):
#   docker build -f services/emr/Dockerfile services
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY gokit/go.mod gokit/go.sum gokit/
COPY emr/go.mod emr/go.sum emr/
WORKDIR /src/emr
RUN go mod download
COPY gokit/ /src/gokit/
COPY emr/ /src/emr/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/emr ./cmd/emr && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/egress-emr ./cmd/egress-emr
# /emr-keys holds the sealing key pair (volume emr-keys), /emr-secrets the
# file secret store (volume emr-secrets): owned by the nonroot user so a
# fresh volume takes the ownership on first mount. /emr-proxy-key holds
# K_proxy (volume emr-proxy-key), /emr-egress the allow-list the emr
# service writes and egress-emr reads (volume emr-egress-conf).
RUN mkdir -p /out/emr-keys /out/emr-secrets /out/emr-proxy-key /out/emr-egress

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/emr /emr
COPY --from=build /out/egress-emr /egress-emr
COPY --from=build --chown=65532:65532 /out/emr-keys /emr-keys
COPY --from=build --chown=65532:65532 /out/emr-secrets /emr-secrets
COPY --from=build --chown=65532:65532 /out/emr-proxy-key /emr-proxy-key
COPY --from=build --chown=65532:65532 /out/emr-egress /emr-egress
USER nonroot:nonroot
ENTRYPOINT ["/emr"]
CMD ["serve"]
