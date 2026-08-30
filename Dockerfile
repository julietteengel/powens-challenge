FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/dispatcher ./cmd/dispatcher
RUN CGO_ENABLED=0 go build -o /out/testreceiver ./cmd/testreceiver

FROM alpine:3.20 AS dispatcher
COPY --from=build /out/dispatcher /usr/local/bin/dispatcher
ENTRYPOINT ["/usr/local/bin/dispatcher"]

FROM alpine:3.20 AS testreceiver
COPY --from=build /out/testreceiver /usr/local/bin/testreceiver
ENTRYPOINT ["/usr/local/bin/testreceiver"]
