FROM localhost:5001/webtop@sha256:e804c4fae587f75703c45e2c07f25cb9fec255cbecf7161654164d3d4166f5b4
# gVisor cannot create a network namespace with a loopback device. glycin (the only image loader in
# this gdk-pixbuf) sandboxes loaders with bwrap --unshare-all, so every icon load fails and xfce4-panel aborts.
# The wrapper keeps the host network namespace and leaves the rest of the sandbox in place.
RUN mv /usr/bin/bwrap /usr/bin/bwrap.real
COPY bwrap-shim.sh /usr/bin/bwrap
RUN chmod 755 /usr/bin/bwrap
