FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/sh tester && passwd -d tester \
    && mkdir -p /run/sshd
COPY sshd_config /etc/ssh/sshm_test_config
EXPOSE 2222
CMD ["/usr/sbin/sshd", "-D", "-e", "-f", "/etc/ssh/sshm_test_config"]
