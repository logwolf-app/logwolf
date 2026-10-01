# The builder runs on the build machine's platform and cross-compiles for the
# target one, so a multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25.4-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

RUN mkdir /app
COPY . /app

WORKDIR /app

WORKDIR ./broker
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o broker ./cmd/api

RUN chmod +x /app/broker

FROM alpine:latest

RUN mkdir /app

COPY --from=builder /app/broker /app

CMD [ "/app/broker" ]
