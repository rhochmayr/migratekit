FROM fedora:43 AS build
RUN dnf install -y golang libnbd-devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /migratekit main.go

# NOTE: Pinned to fedora:43 instead of fedora:44. On fedora:44 the runtime
# invocation of virt-v2v -> supermin fails with:
#   /usr/bin/supermin: symbol lookup error: /lib64/librpm_sequoia.so.1:
#   undefined symbol: EVP_idea_cfb64, version OPENSSL_3.0.0
# This is an ABI mismatch in the Fedora 44 packages: rpm-sequoia is linked
# against an OpenSSL build that exports the IDEA cipher symbols, but the
# openssl-libs shipped in the container does not export them. A plain
# `dnf upgrade --refresh` does not resolve it because the mismatch exists
# in the published packages. Fedora 43 ships a consistent set.
FROM fedora:43
ADD https://fedorapeople.org/groups/virt/virtio-win/virtio-win.repo /etc/yum.repos.d/virtio-win.repo
RUN \
  dnf install -y nbdkit nbdkit-vddk-plugin libnbd virt-v2v virtio-win && \
  dnf clean all && \
  rm -rf /var/cache/dnf
COPY --from=build /migratekit /usr/local/bin/migratekit
ENTRYPOINT ["/usr/local/bin/migratekit"]
