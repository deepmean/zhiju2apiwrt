#!/bin/sh
# Register zhiju2api in /etc/sysupgrade.conf so a firmware upgrade does not wipe the
# deployment. Idempotent: running it twice changes nothing.
#
#   run on the router:   sh keep-on-sysupgrade.sh
#   or from a desktop:   ssh router 'sh -s' < tools/keep-on-sysupgrade.sh
#
# The list is what OpenWrt keeps when it upgrades: files and directories, copied into the
# new root after the image is written. Everything this service owns lives in three places —
# /opt/zhiju2api (binary, conf, tokens, ledger), the init script, and the rc.d symlinks —
# so all of them have to be listed, exactly as the other services on this router do it.

set -eu

CONF=/etc/sysupgrade.conf
PATHS="
/opt/zhiju2api/
/etc/init.d/zhiju2api
/etc/rc.d/S96zhiju2api
/etc/rc.d/K10zhiju2api
"

[ -f "$CONF" ] || { echo "no $CONF on this device" >&2; exit 1; }

added=0
for p in $PATHS; do
	if grep -qxF "$p" "$CONF"; then
		echo "  already listed: $p"
		continue
	fi
	echo "$p" >> "$CONF"
	echo "  added:          $p"
	added=$((added + 1))
done

echo "added $added entr(y|ies) to $CONF"
echo
echo "--- the service's block in $CONF ---"
grep -n 'zhiju' "$CONF" || echo "  (nothing — that would be a bug)"
echo
echo "--- sanity: everything listed actually exists ---"
for p in $PATHS; do
	if [ -e "$p" ]; then
		echo "  ok      $p"
	else
		echo "  MISSING $p   <-- sysupgrade would have nothing to restore here"
	fi
done
