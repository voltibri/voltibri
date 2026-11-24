# syntax=docker/dockerfile:1
# voltibri: single static binary (pure Go SQLite), minimal runtime image.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY sdk/go.mod sdk/
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/voltibri ./cmd/voltibri
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/voltibri /usr/local/bin/voltibri
COPY --from=build --chown=nonroot:nonroot /out/data /data
# database, secret key and caches
VOLUME /data
ENV VOLTIBRI_DATA=/data
EXPOSE 7070
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/voltibri"]
CMD ["serve", "-data", "/data", "-listen", ":7070"]
