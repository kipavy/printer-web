# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /usr/local/bin/printer-web ./cmd/printer-web

FROM alpine:3.20
LABEL org.opencontainers.image.source="https://github.com/kipavy/printer-web" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.description="Print from a phone browser to one network printer"
# ghostscript: PDF/PostScript tools, enscript: text, libheif-tools: HEIC,
# tiff-tools: TIFF. Images are converted natively in Go.
RUN apk add --no-cache ghostscript enscript libheif-tools tiff-tools
COPY --from=build /usr/local/bin/printer-web /usr/local/bin/printer-web
USER 65534:65534
WORKDIR /tmp
ENV HOME=/tmp LISTEN_ADDR=:28431
EXPOSE 28431
ENTRYPOINT ["printer-web"]
