FROM fedora:44 AS build
RUN dnf install -y golang libnbd-devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /migratekit main.go

# Based on fedora:44 GA with updates and updates-testing repos disabled to avoid
# the rpm-sequoia / openssl-libs ABI mismatch (EVP_idea_cfb64 undefined symbol)
# that has been shipping in the updates repo. The GA compose is internally tested
# as a consistent set. The supermin --version smoke test below guards against
# regression.
FROM fedora:44
ADD https://fedorapeople.org/groups/virt/virtio-win/virtio-win.repo /etc/yum.repos.d/virtio-win.repo
RUN \
  dnf install -y --disablerepo=updates --disablerepo=updates-testing \
    nbdkit nbdkit-vddk-plugin libnbd virt-v2v virtio-win && \
  dnf clean all && \
  rm -rf /var/cache/dnf
# Fail the build immediately if the librpm_sequoia / openssl-libs ABI mismatch
# that causes "undefined symbol: EVP_idea_cfb64" is present in the installed
# package set. Running supermin triggers the dynamic linker and surfaces the
# mismatch without needing to actually build an appliance.
RUN /usr/bin/supermin --version
COPY --from=build /migratekit /usr/local/bin/migratekit
ENTRYPOINT ["/usr/local/bin/migratekit"]
