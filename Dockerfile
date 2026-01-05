FROM golang:alpine AS builder

RUN apk add build-base

WORKDIR mensaapp_server

COPY . .


ARG CGO_ENABLED=1
ARG GOOS=linux
ARG GOARCH=amd64
ARG CGO_CFLAGS="-D_LARGEFILE64_SOURCE"

RUN go get ./...
RUN go install ./...

RUN  go build -o /main ./main


FROM alpine:latest AS deploy

WORKDIR /

RUN apk --no-cache add tzdata

RUN mkdir "./pb"

COPY --from=builder /main ./pb/main/main

EXPOSE 8080
CMD ["/pb/main/main", "serve", "--http=0.0.0.0:8080"]