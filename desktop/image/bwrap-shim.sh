#!/bin/sh
# gVisor cannot set up a loopback device in a new network namespace, so keep the host one.
# --share-net must follow --unshare-all, so insert it right after.
n=$#
i=0
while [ $i -lt $n ]; do
  a=$1
  shift
  set -- "$@" "$a"
  if [ "$a" = "--unshare-all" ]; then set -- "$@" --share-net; fi
  i=$((i+1))
done
exec /usr/bin/bwrap.real "$@"
