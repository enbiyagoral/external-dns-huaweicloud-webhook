FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum /build/
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /external-dns-huaweicloud ./cmd/webhook

FROM alpine:latest

COPY --from=builder --chown=root:root /external-dns-huaweicloud /bin/

USER nobody
CMD ["/bin/external-dns-huaweicloud"]
