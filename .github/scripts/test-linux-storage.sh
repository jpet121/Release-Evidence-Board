#!/usr/bin/env bash
set -euo pipefail

# The disk-full test checks that this mount starts empty and has exactly 16 MiB.
fault_dir=$(mktemp -d /tmp/reb-enospc.XXXXXXXX)
cleanup() {
    if mountpoint -q -- "$fault_dir"; then
        sudo umount -- "$fault_dir" || return 1
    fi
    rmdir -- "$fault_dir"
}
trap cleanup EXIT

sudo mount -t tmpfs \
    -o "size=16M,mode=0700,uid=$(id -u),gid=$(id -g),nodev,nosuid,noexec" \
    tmpfs "$fault_dir"
REB_PRIVATE_ENOSPC_ROOT="$fault_dir" go test -tags=reb_storage_fault -count=1 ./...
