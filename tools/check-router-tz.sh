#!/bin/sh
# Why does the gateway log UTC while the router is CST? Read the process environment.
PID=$(pidof zhiju2api | awk '{print $1}')
echo "pid=$PID"
if [ -z "$PID" ]; then echo "not running"; exit 1; fi
echo "--- env (TZ/GOGC/LISTEN) ---"
tr '\0' '\n' < /proc/$PID/environ | grep -E '^(TZ|GOGC|LISTEN)='
echo "--- /etc/TZ ---"
cat /etc/TZ
echo
echo "--- router date vs a child of the same procd instance ---"
date
echo "--- init script env lines ---"
grep -n 'procd_set_param env' /etc/init.d/zhiju2api
echo "--- last log timestamps (gateway writes its own) ---"
logread -e zhiju2api | tail -2
