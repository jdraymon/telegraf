FROM buildpack-deps:bullseye-curl

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get upgrade -y && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends iputils-ping snmp procps lm-sensors libcap2-bin && \
    rm -rf /var/lib/apt/lists/*

COPY build/dist/telegraf_1.23.0~bfa2e66e-0_amd64.deb .
RUN ARCH= && dpkgArch="$(dpkg --print-architecture)" && \
    case "${dpkgArch##*-}" in \
      amd64) ARCH='amd64';; \
      arm64) ARCH='arm64';; \
      armhf) ARCH='armhf';; \
      armel) ARCH='armel';; \
      *)     echo "Unsupported architecture: ${dpkgArch}"; exit 1;; \
    esac && \
    dpkg -i telegraf_1.23.0~bfa2e66e-0_amd64.deb && \
    rm -f telegraf_1.23.0~bfa2e66e-0_amd64.deb*

EXPOSE 8125/udp 8092/udp 8094

COPY docker/entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
CMD ["telegraf"]

