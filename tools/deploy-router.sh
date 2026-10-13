#!/bin/sh
# Deploy zhiju2api to an OpenWrt router over ssh.
#
# The binary is replaced; the configuration is NOT touched unless you ask for it. That
# default matters: the config on the router is the operator's (API key, token file
# paths, resolution tier), and a deploy that silently overwrites it is how you end up
# debugging a "wrong API key" that nobody typed.
#
#   tools/deploy-router.sh jcg                    # binary only
#   tools/deploy-router.sh jcg --conf             # also push deploy/zhiju2api.conf.example
#   tools/deploy-router.sh jcg --binary ./dist/zhiju2api-linux-mipsle
#
# Notes baked in from real deployments:
#   * the running service holds the binary open, so it is stopped before the upload and
#     started again afterwards (a plain scp over a live binary fails with "dest open");
#   * procd reads the conf once at start, so a conf change needs the restart this script
#     performs anyway;
#   * the upload is verified by md5 and the config by a key-length check, because both
#     have silently gone wrong before.

set -eu

HOST="${1:-jcg}"
shift || true
BINARY="./dist/zhiju2api-linux-mipsle"
WANT_CONF=0
CONF="./deploy/zhiju2api.conf.example"
ADDR=""

while [ $# -gt 0 ]; do
	case "$1" in
		--conf) WANT_CONF=1 ;;
		--binary) shift; BINARY="$1" ;;
		--conf-file) shift; CONF="$1"; WANT_CONF=1 ;;
		--addr) shift; ADDR="$1" ;;
		*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
	shift
done

[ -f "$BINARY" ] || { echo "binary not found: $BINARY" >&2; exit 1; }

LOCAL_MD5=$(md5sum "$BINARY" | awk '{print $1}')
echo "==> deploying $BINARY ($LOCAL_MD5) to $HOST"

echo "==> stopping the service"
ssh -o BatchMode=yes "$HOST" "/etc/init.d/zhiju2api stop 2>/dev/null || true; sleep 2; rm -f /opt/zhiju2api/zhiju2api"

echo "==> uploading"
scp -q "$BINARY" "$HOST:/opt/zhiju2api/zhiju2api"

if [ "$WANT_CONF" = "1" ]; then
	echo "==> uploading config $CONF (this REPLACES the router's own config)"
	scp -q "$CONF" "$HOST:/opt/zhiju2api/zhiju2api.conf"
	ssh -o BatchMode=yes "$HOST" "chmod 600 /opt/zhiju2api/zhiju2api.conf"
fi

echo "==> verifying and starting"
ssh -o BatchMode=yes "$HOST" "
	set -e
	chmod 755 /opt/zhiju2api/zhiju2api
	REMOTE_MD5=\$(md5sum /opt/zhiju2api/zhiju2api | awk '{print \$1}')
	[ \"\$REMOTE_MD5\" = \"$LOCAL_MD5\" ] || { echo \"md5 mismatch: \$REMOTE_MD5\" >&2; exit 1; }
	KEYLEN=\$(awk -F= '/^API_KEY/{print length(\$2)}' /opt/zhiju2api/zhiju2api.conf || echo 0)
	echo \"    config API_KEY length: \$KEYLEN\"
	/etc/init.d/zhiju2api start
	sleep 12
	logread -e zhiju2api | tail -6
	echo '--- listening ---'
	netstat -ltn | grep 8080 || echo '    NOT listening'
"

echo "==> done."
if [ -n "${ADDR:-}" ]; then
	KEY=$(awk -F= '/^API_KEY/{print $2}' /opt/zhiju2api/zhiju2api.conf 2>/dev/null || true)
	echo "==> health:"
	curl -s -m 20 -H "Authorization: Bearer ${KEY:-}" "http://$ADDR:8080/health" | head -c 400 || true
	echo
else
	echo "    (pass --addr <router-ip> to also probe /health from here)"
fi
