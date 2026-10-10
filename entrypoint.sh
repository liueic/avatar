#!/bin/sh
set -e

# If the first argument is a flag (starts with '-'), prepend the binary.
if [ "${1#-}" != "$1" ]; then
    set -- /usr/local/bin/avater "$@"
fi

# When running as root, fix ownership of /data and drop privileges to user 'avater'.
if [ "$(id -u)" = '0' ]; then
    mkdir -p /data
    chown -R avater:avater /data
    exec gosu avater "$@"
fi

exec "$@"
