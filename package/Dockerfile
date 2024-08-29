FROM registry.suse.com/bci/bci-micro:15.6 AS final

# Temporary build stage image
FROM registry.suse.com/bci/bci-base:15.6 AS builder

# Install system packages using builder image that has zypper
COPY --from=final / /chroot/

RUN zypper refresh && \
    zypper --installroot /chroot -n in --no-recommends \
    openssl patterns-base-fips && \
    zypper --installroot /chroot clean -a && \
    rm -rf /chroot/var/cache/zypp/* /chroot/var/log/zypp/* /chroot/tmp/* /chroot/var/tmp/* /chroot/usr/share/doc/packages/*

# Main stage using bci-micro as the base image
FROM final

# Copy binaries and configuration files from builder to micro
COPY --from=builder /chroot/ /

COPY bin/rancher-system-agent /usr/bin/
RUN chmod +x /usr/bin/rancher-system-agent
CMD ["rancher-system-agent"]
