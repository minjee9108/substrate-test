FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      openssh-server openssh-sftp-server git ca-certificates curl gh procps less \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd \
 && rm -f /etc/ssh/ssh_host_* \
 && useradd -m -u 1000 -s /bin/bash dev && usermod -p '*' dev
USER dev
RUN curl -fsSL https://claude.ai/install.sh | bash
USER root
# The home directory is a PVC, which hides the install above. The entrypoint seeds the PVC from this copy.
RUN cp -a /home/dev /opt/dev-skel && ln -s /home/dev/.local/bin/claude /usr/local/bin/claude
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
