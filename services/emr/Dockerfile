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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/emr /emr
USER nonroot:nonroot
ENTRYPOINT ["/emr"]
CMD ["serve"]
