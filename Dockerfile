# build stage
FROM golang:1.17.1-alpine3.14 AS build

COPY . /go/src/github.com/gjtempleton/spot-termination-exporter
WORKDIR /go/src/github.com/gjtempleton/spot-termination-exporter
RUN go build -o /bin/spot-termination-exporter .

FROM alpine:3.14
RUN apk update && apk add ca-certificates && rm -rf /var/cache/apk/*
COPY --from=build /bin/spot-termination-exporter /bin

USER nobody

ENTRYPOINT ["/bin/spot-termination-exporter"]
