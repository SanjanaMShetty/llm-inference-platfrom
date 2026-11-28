# Fake backend image. The fake backend uses only the Go standard library, so no
# module download step is needed. Keep GO_VERSION in step with go.mod.
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fakebackend ./cmd/fakebackend

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fakebackend /fakebackend
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/fakebackend"]
