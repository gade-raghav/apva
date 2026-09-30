# Copyright 2026 Raghav Gade
# SPDX-License-Identifier: Apache-2.0
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/apva ./cmd/apva

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/apva /apva
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/apva"]
