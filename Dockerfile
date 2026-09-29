ARG GO_VERSION=1.27.1

FROM golang:${GO_VERSION}-alpine AS builder

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA} -X main.buildDate=${BUILD_DATE}" \
    -o /pushward ./cmd/pushward

# The root variant of distroless/static, on purpose. This image is also the
# runtime of the pushward GitHub Action, and GitHub runs Docker actions as the
# image's user: anything but root cannot write $GITHUB_OUTPUT or the mounted
# workspace. The binary is a one-shot client with no listener. For plain
# docker run, pass --user 65532 to drop root.
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2

ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="pushward-cli" \
      org.opencontainers.image.description="Command-line client for PushWard notifications, Live Activities and widgets" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/mac-lucky/pushward-cli" \
      org.opencontainers.image.licenses="MIT"

COPY --from=builder /pushward /pushward

ENTRYPOINT ["/pushward"]
