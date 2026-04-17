FROM fedora:44 AS build
RUN dnf install -y golang libnbd-devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /migratekit main.go

FROM fedora:44
ADD https://fedorapeople.org/groups/virt/virtio-win/virtio-win.repo /etc/yum.repos.d/virtio-win.repo
RUN dnf install -y \
      nbdkit nbdkit-vddk-plugin libnbd virt-v2v virtio-win && \
    dnf clean all && \
    rm -rf /var/cache/dnf

# Build-time smoke test: invoking supermin triggers the dynamic linker and
# surfaces any librpm_sequoia / openssl-libs ABI mismatch (undefined symbol
# EVP_idea_cfb64 etc.) during `docker build` instead of at migration time.
RUN /usr/bin/supermin --version

COPY --from=build /migratekit /usr/local/bin/migratekit
ENTRYPOINT ["/usr/local/bin/migratekit"]
