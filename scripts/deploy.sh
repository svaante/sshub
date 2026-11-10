#!/bin/sh
# Build sshub here and install it on another machine over ssh, as a server
# or a client: the same as cloning there and running scripts/install.sh, but
# without Go on the target. Asks for the sudo password on the target.
#
# usage: scripts/deploy.sh server|client <host> [arch]    (arch defaults to arm64)
#
# Either role is set up for the user ssh logs in as (put in the sshub group).
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
	echo "usage: $0 server|client <host> [arch]" >&2
	exit 2
fi
role=$1
host=$2
arch=${3:-arm64}
case $role in
server | client) ;;
*)
	echo "$0: role must be server or client, not $role" >&2
	exit 2
	;;
esac

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

# A copy of the checkout with the binaries built, as install.sh expects.
cd "$(dirname "$0")/.."
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$out/" ./cmd/sshub ./cmd/sshub-server
mkdir -p "$out/helper" "$out/scripts" "$out/packaging"
cp helper/sshub-helper "$out/helper/"
cp scripts/install.sh "$out/scripts/"
cp -R packaging/sudoers "$out/packaging/"

# Copy over plain ssh rather than scp, which needs sftp-server on the host.
# COPYFILE_DISABLE keeps macOS tar from adding ._* metadata files.
remote=$(ssh "$host" mktemp -d)
COPYFILE_DISABLE=1 tar -C "$out" -cf - . | ssh "$host" "tar -C $remote -xf -"
ssh -t "$host" "sudo sh $remote/scripts/install.sh $role; s=\$?; rm -rf $remote; exit \$s"
