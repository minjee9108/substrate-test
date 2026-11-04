FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /sshbridge .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server openssh-sftp-server git ca-certificates curl gh procps less \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd \
 && rm -f /etc/ssh/ssh_host_* \
 && useradd -m -s /bin/bash dev && usermod -p '*' dev \
 && mkdir -p /home/dev/.ssh && chown dev:dev /home/dev/.ssh && chmod 700 /home/dev/.ssh
USER dev
RUN curl -fsSL https://claude.ai/install.sh | bash && ls -la /home/dev/.local/bin
ENV PATH=/home/dev/.local/bin:$PATH
USER root
RUN ln -s /home/dev/.local/bin/claude /usr/local/bin/claude
COPY authorized_keys /home/dev/.ssh/authorized_keys
RUN chown dev:dev /home/dev/.ssh/authorized_keys && chmod 600 /home/dev/.ssh/authorized_keys
COPY sshd_config /etc/ssh/sshd_config
COPY --from=build /sshbridge /sshbridge
ENTRYPOINT ["/sshbridge"]
