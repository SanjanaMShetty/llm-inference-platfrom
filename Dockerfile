FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fakebackend ./cmd/fakebackend

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/fakebackend /fakebackend
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/fakebackend"]
