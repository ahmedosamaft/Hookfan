# Stage 1 — build a fully static binary.
FROM golang:1.25-alpine AS build

# Set by buildx during a multi-platform build. They are left empty by a plain
# `docker build`, and an empty GOOS/GOARCH means "use the toolchain default" —
# which is the host architecture, exactly what a single-platform build wants.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Copy the module files first so dependency download is cached independently of
# source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# CGO off and -s -w (strip symbol table and DWARF) keep the binary small enough
# to sit comfortably under the 20MB image target.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/hookfan \
      ./cmd/hookfan

# Stage 2 — distroless: no shell, no package manager, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/hookfan /hookfan

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/hookfan"]
