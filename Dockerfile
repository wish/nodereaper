FROM --platform=$BUILDPLATFORM golang:1.12

ARG BUILDPLATFORM
ARG TARGETARCH
ARG TARGETOS

ENV GO111MODULE=on
WORKDIR /go/src/github.com/wish/nodereaper

# Cache dependencies
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . /go/src/github.com/wish/nodereaper/

# Build controller
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=${TARGETOS} go build -o ./nodereaper/nodereaper -a -installsuffix cgo ./nodereaper

# Build daemon
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=${TARGETOS} go build -o ./nodereaperd/nodereaperd -a -installsuffix cgo ./nodereaperd

FROM alpine:3.7
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=0 /go/src/github.com/wish/nodereaper/nodereaper/nodereaper /root/nodereaper
COPY --from=0 /go/src/github.com/wish/nodereaper/nodereaperd/nodereaperd /root/nodereaperd
