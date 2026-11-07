# The emr service (ADR 0014). Built from services/ (it requires
# services/gokit):
#   docker build -f services/emr/Dockerfile services
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY gokit/go.mod gokit/go.sum gokit/
COPY emr/go.mod emr/go.sum emr/
WORKDIR /src/emr
RUN go mod download
COPY gokit/ /src/gokit/
COPY emr/ /src/emr/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/emr ./cmd/emr
# /emr-keys holds the sealing key pair (volume emr-keys), /emr-secrets the
# file secret store (volume emr-secrets): owned by the nonroot user so a
# fresh volume takes the ownership on first mount.
RUN mkdir -p /out/emr-keys /out/emr-secrets

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/emr /emr
COPY --from=build --chown=65532:65532 /out/emr-keys /emr-keys
COPY --from=build --chown=65532:65532 /out/emr-secrets /emr-secrets
USER nonroot:nonroot
ENTRYPOINT ["/emr"]
CMD ["serve"]
