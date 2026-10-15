# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go test ./... \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/collector-agent .

FROM scratch
COPY --from=build /out/collector-agent /collector-agent
EXPOSE 9100
USER 65534:65534
ENTRYPOINT ["/collector-agent"]

