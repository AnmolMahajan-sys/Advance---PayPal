# Build
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/advance ./cmd/advance

# Run: static binary on a minimal base, non-root
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/advance /advance
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/advance"]
