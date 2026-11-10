#!/bin/sh
# Install sshub on this machine, from a checkout where the binaries are built
# (go build -o . ./cmd/...). Run with sudo:
#
#   install.sh [-n] server [user]   sshub-server + helper, usbip-host,
#                                   /etc/sshub, user (default: who ran sudo)
#                                   in the sshub group
#   install.sh [-n] client [user]   sshub + helper, vhci-hcd, user in the
#                                   sshub group
#   install.sh [-n] uninstall       remove everything except /etc/sshub
#
#   -n  only show the commands that would change the system; needs no root
#
# Every command that changes the system is shown, prefixed with "+", as it
# runs. server and client keep a copy of this script as
# /usr/libexec/sshub/install.sh, for uninstall. Running it again is safe: the
# devices list is kept.
set -eu

# The checkout: this script lives in its scripts/ directory.
self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
src=$(cd "$(dirname "$0")/.." && pwd)
bindir=/usr/local/bin
libexec=/usr/libexec/sshub
conf=/etc/sshub
dry= # set by -n

die() {
	echo "install.sh: $*" >&2
	exit 1
}

note() {
	echo "install.sh: $*"
}

usage() {
	sed -n '5,12s/^# \{0,1\}//p' "$0" >&2
	exit 2
}

# quote WORD...: the words as they could be typed into a shell.
quote() {
	q=
	for a; do
		case $a in
		'' | *[!A-Za-z0-9_./:=@%+,-]*)
			a="'$(printf '%s' "$a" | sed "s/'/'\\\\''/g")'"
			;;
		esac
		q="${q:+$q }$a"
	done
	printf '%s' "$q"
}

# run CMD...: show CMD, then run it unless -n. Everything that changes the
# system goes through run or put.
run() {
	printf '+ %s\n' "$(quote "$@")"
	if [ -z "$dry" ]; then
		"$@"
	fi
}

# put [-a] FILE LINE: write LINE to FILE, or append it with -a; shown like run.
put() {
	redir='>'
	if [ "$1" = -a ]; then
		redir='>>'
		shift
	fi
	printf '+ %s %s %s\n' "$(quote echo "$2")" "$redir" "$(quote "$1")"
	if [ -n "$dry" ]; then
		return
	fi
	if [ "$redir" = '>>' ]; then
		printf '%s\n' "$2" >>"$1"
	else
		printf '%s\n' "$2" >"$1"
	fi
}

# done_note TEXT: report success, or in a dry run that nothing changed.
done_note() {
	if [ -n "$dry" ]; then
		note "dry run (-n): nothing was changed"
	else
		note "$*"
	fi
}

# need PROGRAM: the checkout must have PROGRAM built.
need() {
	[ -e "$src/helper/sshub-helper" ] || die "$src is not an sshub checkout; run scripts/install.sh from one"
	[ -e "$src/$1" ] || die "$src/$1 missing; build first: go build -o . ./cmd/..."
}

# install_files PROGRAM: PROGRAM, the helper, and this script.
install_files() {
	run install -D -o root -g root -m 0755 "$src/$1" "$bindir/$1"
	run install -D -o root -g root -m 0755 "$src/helper/sshub-helper" "$libexec/sshub-helper"
	run install -o root -g root -m 0755 "$self" "$libexec/install.sh"
}

# install_sudoers FILE: check FILE, then install it as /etc/sudoers.d/sshub.
# A broken sudoers file can lock everyone out of sudo.
install_sudoers() {
	visudo -cf "$1" >/dev/null || die "$1: invalid sudoers file"
	run install -o root -g root -m 0440 "$1" /etc/sudoers.d/sshub
}

# load_module NAME: load the kernel module now and at every boot.
load_module() {
	put /etc/modules-load.d/sshub.conf "$1"
	run modprobe "$1"
}

# add_user USER: create the sshub group if needed and put USER in it.
add_user() {
	if ! getent group sshub >/dev/null; then
		run groupadd --system sshub
	fi
	run usermod -aG sshub "$1"
}

# check_user USER ROLE: USER must be given and exist.
check_user() {
	[ -n "$1" ] || die "usage: install.sh $2 <user> (no user given, and not run via sudo)"
	getent passwd "$1" >/dev/null || die "no such user: $1"
}

server() {
	user=${1:-}
	check_user "$user" server
	need sshub-server
	install_files sshub-server
	load_module usbip-host
	add_user "$user"
	run install -d -o root -g root -m 0755 "$conf"
	if [ ! -e "$conf/devices" ]; then
		put "$conf/devices" '# vid:pid  [serial]    e.g. "0483:374b" or "303a:1001  A0:F2:62:69:68:E0"'
	fi
	# sshub-server refuses a devices file anyone but root could change.
	run chown root:root "$conf/devices"
	run chmod 0644 "$conf/devices"
	install_sudoers "$src/packaging/sudoers/server"
	done_note "server installed; list what to share in $conf/devices"
}

client() {
	user=${1:-}
	check_user "$user" client
	need sshub
	install_files sshub
	load_module vhci-hcd
	if [ ! -x /usr/bin/usbip ]; then
		note "warning: /usr/bin/usbip not found (Ubuntu: apt install linux-tools-generic)"
	fi
	add_user "$user"
	install_sudoers "$src/packaging/sudoers/client"
	done_note "client installed; $user must log in again to be in the sshub group"
}

uninstall() {
	run rm -f /etc/sudoers.d/sshub /etc/modules-load.d/sshub.conf \
		"$bindir/sshub" "$bindir/sshub-server"
	if getent group sshub >/dev/null; then
		run groupdel sshub
	fi
	# Fails while devices are exported or attached; they keep working
	# until then.
	run modprobe -r usbip-host 2>/dev/null || :
	run modprobe -r vhci-hcd 2>/dev/null || :
	run rm -rf "$libexec"
	if [ -d "$conf" ]; then
		note "kept $conf (the devices list); remove it by hand if no longer needed"
	fi
	done_note "uninstalled"
}

if [ "${1:-}" = -n ]; then
	dry=1
	shift
fi
[ -n "$dry" ] || [ "$(id -u)" = 0 ] || die "run as root (or with -n to only show what it would do)"
# The user to put in the sshub group: who ran sudo, or for -n without sudo,
# whoever runs it.
me=${SUDO_USER:-}
if [ -z "$me" ] && [ -n "$dry" ]; then
	me=$(id -un)
fi
case ${1:-} in
server) server "${2:-$me}" ;;
client) client "${2:-$me}" ;;
uninstall) uninstall ;;
*) usage ;;
esac
