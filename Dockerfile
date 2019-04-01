FROM golang:1.11 AS builder
ADD https://github.com/golang/dep/releases/download/v0.5.1/dep-linux-amd64 /usr/bin/dep
RUN chmod +x /usr/bin/dep
WORKDIR $GOPATH/src/github.com/borgkun/elastic-exporter
COPY Gopkg.toml Gopkg.lock main.go ./
RUN dep ensure --vendor-only
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix nocgo -o elasticExporter

# final stage
FROM alpine
WORKDIR /app
COPY --from=builder /go/src/github.com/borgkun/elastic-exporter/elasticExporter /app/elasticExporter
COPY  settings.yaml /app/settings.yaml
EXPOSE 8092
CMD /app/elasticExporter