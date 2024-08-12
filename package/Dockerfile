FROM registry.suse.com/bci/bci-micro:15.6

RUN zypper in -y openssl patterns-base-fips

COPY bin/rancher-system-agent /usr/bin/
RUN chmod +x /usr/bin/rancher-system-agent
CMD ["rancher-system-agent"]
