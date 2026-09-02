FROM golang:alpine AS builder

# Add ca-certs
RUN apk add --update --no-cache ca-certificates

# Set necessary environmet variables needed for our image
ENV GO111MODULE=on \
    CGO_ENABLED=0

ADD . /build
WORKDIR /build

# Build metadata, injected into github.com/prometheus/common/version so that
# vmware_exporter_build_info carries real values instead of empty strings.
#
# These are ARGs rather than derived in-image because the Dockerfile has no
# access to git metadata: `ADD . /build` copies the working tree and
# .dockerignore excludes .git. The caller supplies them:
#
#   docker build --build-arg VERSION=0.2.0 \
#                --build-arg REVISION=$(git rev-parse HEAD) .
#
# Left unset, build_info falls back to empty values -- the same state as before
# this change, not a build failure. That is deliberate: a forgotten
# --build-arg should not stop somebody from building the image locally.
#
# Note that `revision` looks populated even without any ldflags, because Go
# stamps it from VCS on its own. That is a trap: it makes build_info appear to
# work while version and branch are still empty.
ARG VERSION=""
ARG REVISION=""
ARG BRANCH=""
ARG BUILD_DATE=""

# Build the package, not a file list.
#
# `go build ... vmware-exporter.go` compiles only the files named on the command
# line. Adding a second file to package main would not break the build -- the
# new file would just be dropped from the binary, silently. Verified: with a
# second main-package file containing an init(), the file-list form produced a
# binary where that init never ran; `go build .` produced one where it did.
#
# -s -w strip the symbol table and DWARF data, matching .goreleaser.yaml.
RUN go mod download && \
    go build -a -ldflags "-s -w -extldflags '-static' \
      -X github.com/prometheus/common/version.Version=${VERSION} \
      -X github.com/prometheus/common/version.Revision=${REVISION} \
      -X github.com/prometheus/common/version.Branch=${BRANCH} \
      -X github.com/prometheus/common/version.BuildDate=${BUILD_DATE} \
      -X github.com/prometheus/common/version.BuildUser=docker" \
      -o vmware-exporter .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /build/vmware-exporter /bin/vmware-exporter

EXPOSE 9169

ENTRYPOINT [ "/bin/vmware-exporter" ]
