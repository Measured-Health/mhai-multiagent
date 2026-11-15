# The documents service (ADR 0016). Built from services/ (it requires
# services/gokit):
#   docker build -f services/documents/Dockerfile services
# Distroless and non-root; compose runs it with a read-only root, every
# capability dropped and no-new-privileges, on the internal network only.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY gokit/go.mod gokit/go.sum gokit/
COPY documents/go.mod documents/go.sum documents/
WORKDIR /src/documents
RUN go mod download
COPY gokit/ /src/gokit/
COPY documents/ /src/documents/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/documents ./cmd/documents

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/documents /documents
USER nonroot:nonroot
ENTRYPOINT ["/documents"]
CMD ["serve"]
