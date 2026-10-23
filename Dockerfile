# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sgpe ./cmd/sgpe

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="sg-property-engine" \
      org.opencontainers.image.source="https://github.com/Cyprinus12138/sg-property-engine" \
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0" \
      org.opencontainers.image.authors="Mr.Cyprinus"
COPY --from=build /out/sgpe /sgpe
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/sgpe"]
CMD ["serve"]
