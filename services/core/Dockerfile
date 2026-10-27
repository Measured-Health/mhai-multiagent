# The core service. Also runs one-shot migrations: core migrate up.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/core /core
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/core"]
CMD ["serve"]
