FROM golang:alpine AS builder

# Add ca-certs
RUN apk add --update --no-cache ca-certificates

# Set necessary environmet variables needed for our image
ENV GO111MODULE=on \
    CGO_ENABLED=0

ADD . /build
WORKDIR /build

# Build the package, not a file list.
#
# `go build ... vmware-exporter.go` compiles only the files named on the command
# line. Adding a second file to package main would not break the build -- the
# new file would just be dropped from the binary, silently. Verified: with a
# second main-package file containing an init(), the file-list form produced a
# binary where that init never ran; `go build .` produced one where it did.
RUN go mod download && \
    go build -a -ldflags '-extldflags "-static"' -o vmware-exporter .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /build/vmware-exporter /bin/vmware-exporter

EXPOSE 9169

ENTRYPOINT [ "/bin/vmware-exporter" ]
