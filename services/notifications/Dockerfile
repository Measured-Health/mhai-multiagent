# The notifications service (ADR 0018). Built from services/ (it requires
# services/gokit):
#   docker build -f services/notifications/Dockerfile services
# Distroless and non-root; compose runs it with a read-only root, every
# capability dropped and no-new-privileges, on the internal network only.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY gokit/go.mod gokit/go.sum gokit/
COPY notifications/go.mod notifications/go.sum notifications/
WORKDIR /src/notifications
RUN go mod download
COPY gokit/ /src/gokit/
COPY notifications/ /src/notifications/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/notifications ./cmd/notifications

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/notifications /notifications
USER nonroot:nonroot
ENTRYPOINT ["/notifications"]
CMD ["serve"]
