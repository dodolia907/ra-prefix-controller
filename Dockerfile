FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /ra-prefix-controller .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/dodolia907/ra-prefix-controller"
COPY --from=build /ra-prefix-controller /ra-prefix-controller
ENTRYPOINT ["/ra-prefix-controller"]
