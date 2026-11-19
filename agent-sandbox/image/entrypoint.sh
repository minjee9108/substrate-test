#!/bin/bash
set -eu
# Host key lives on its own volume so it survives suspend and resume, and the dev user cannot read it.
if [ ! -f /var/lib/sshd-host/ssh_host_ed25519_key ]; then
  ssh-keygen -q -t ed25519 -N '' -f /var/lib/sshd-host/ssh_host_ed25519_key
  echo "generated host key: $(ssh-keygen -lf /var/lib/sshd-host/ssh_host_ed25519_key.pub)"
fi
# First start on an empty home volume: copy the image's home (Claude Code install) into it.
if [ ! -e /home/dev/.seeded ]; then
  cp -a /opt/dev-skel/. /home/dev/
  touch /home/dev/.seeded
  chown dev:dev /home/dev /home/dev/.seeded
fi
exec /usr/sbin/sshd -D -e
