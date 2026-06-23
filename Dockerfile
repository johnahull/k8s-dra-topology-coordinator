FROM golang:1.26 AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w -extldflags=-static" -trimpath -o /bin/nodepartition-controller ./cmd/nodepartition-controller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/nodepartition-controller /usr/local/bin/nodepartition-controller
ENTRYPOINT ["nodepartition-controller"]
