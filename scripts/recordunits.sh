#!/usr/bin/env bash
# Records how the user's systemd manager reports five ways a unit ends, for localhost's tests.
# Units are transient (systemd-run --user) and gone afterwards; nothing is installed.
# Writes testdata/systemd/stops.json, or the path given, from the repository root.
set -euo pipefail
cd "$(dirname "$0")/.."

out=${1:-testdata/systemd/stops.json}
prefix=wayseer-rec
tmp=$(mktemp -d)
trap 'systemctl --user stop "$prefix-*" 2>/dev/null; systemctl --user reset-failed "$prefix-*" 2>/dev/null; rm -rf "$tmp"' EXIT

# A process that takes a second to exit on SIGTERM, so a stop job is seen in the listing.
slow='trap "sleep 1; exit 0" TERM; while :; do sleep 0.2; done'

unit_path() { busctl --user --json=short call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager GetUnit s "$1" | jq -r '.data[0]'; }

# props prints the Unit and Service properties localhost reads, as busctl's JSON.
props() {
	local p
	p=$(unit_path "$1") || { echo null; return; }
	jq -n --argjson u "$(busctl --user --json=short get-property org.freedesktop.systemd1 "$p" org.freedesktop.systemd1.Unit \
		ActiveState SubState UnitFileState ActiveEnterTimestamp InactiveEnterTimestamp TriggeredBy | jq -s .)" \
		--argjson s "$(busctl --user --json=short get-property org.freedesktop.systemd1 "$p" org.freedesktop.systemd1.Service \
			Type RemainAfterExit Result ExecMainCode ExecMainStatus 2>/dev/null | jq -s . || echo '[]')" \
		'[["ActiveState","SubState","UnitFileState","ActiveEnterTimestamp","InactiveEnterTimestamp","TriggeredBy"], $u] as [$un,$uv]
		| [["Type","RemainAfterExit","Result","ExecMainCode","ExecMainStatus"], $s] as [$sn,$sv]
		| ([range(0; $uv|length) | {($un[.]): $uv[.]}] + [range(0; $sv|length) | {($sn[.]): $sv[.]}]) | add'
}

# listing prints the unit's ListUnits entry, or null when the manager no longer has it loaded.
listing() {
	busctl --user --json=short call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager \
		ListUnitsByNames as 1 "$1" | jq '.data[0][0] | if .[2] == "not-found" then null else . end'
}

# hold keeps a unit loaded after it stops, as a target that wants an enabled unit does; After=
# references it without starting it again.
hold() { systemd-run --user --quiet --unit="$prefix-hold-${1//./-}" --property=After="$1" sleep 600; }

main_pid() { systemctl --user show -P MainPID "$1"; }

# wait_until waits up to 10s for the unit's active state to be one of the given ones.
wait_until() {
	local u=$1; shift
	for _ in $(seq 100); do
		local s; s=$(systemctl --user show -P ActiveState "$u")
		for want in "$@"; do [[ $s == "$want" ]] && return 0; done
		sleep 0.1
	done
	echo "$u never reached $*" >&2; return 1
}

# journal prints the manager's and the unit's journal lines since the cursor, as JSON lines.
journal() {
	journalctl --user --output=json --after-cursor="$2" --no-pager \
		| jq -c --arg u "$1" 'select(.USER_UNIT == $u or ._SYSTEMD_USER_UNIT == $u)
			| {__CURSOR, __REALTIME_TIMESTAMP, PRIORITY, _PID, SYSLOG_IDENTIFIER, MESSAGE, MESSAGE_ID, USER_UNIT, _SYSTEMD_USER_UNIT, JOB_TYPE, JOB_RESULT, UNIT_RESULT, EXIT_CODE, EXIT_STATUS}
			| with_entries(select(.value != null))' | jq -s .
}

cursor() { journalctl --user --output=json -n 1 --no-pager | jq -r .__CURSOR; }

# record runs a scenario: start with start_args, act with the named function, and keep what was seen.
record() {
	local name=$1 act=$2; shift 2
	local u=$prefix-$name.service c
	c=$(cursor)
	systemd-run --user --quiet --no-block --unit="$u" "$@"
	hold "$u"
	wait_until "$u" active activating inactive
	local before during after
	before=$(props "$u")
	during=$($act "$u")
	wait_until "$u" inactive failed
	after=$(props "$u")
	sleep 0.5
	jq -n --arg name "$name" --argjson before "$before" --argjson during "$during" --argjson after "$after" \
		--argjson listed "$(listing "$u")" --argjson journal "$(journal "$u" "$c")" \
		'{($name): {before: $before, during: $during, after: $after, listed_after: $listed, journal: $journal}}' >"$tmp/$name.json"
}

# Each act returns the listing seen while the unit ends, or null.
do_stop() { systemctl --user stop --no-block "$1"; sleep 0.3; listing "$1"; }
do_kill() { kill -TERM "$(main_pid "$1")"; sleep 0.3; listing "$1"; }
do_crash() { kill -KILL "$(main_pid "$1")"; echo null; }
do_wait() { echo null; }

record stop do_stop sh -c "$slow"
record kill do_kill sh -c "$slow"
record crash do_crash sh -c "$slow"
record oneshot do_wait --property=Type=oneshot sh -c "sleep 1"

# A timer's service: the timer fires once, a second after it starts.
c=$(cursor)
systemd-run --user --quiet --unit=$prefix-timer --on-active=1s --property=Type=oneshot sh -c "sleep 1"
hold $prefix-timer.service
hold $prefix-timer.timer
sleep 2.5
wait_until $prefix-timer.service inactive
sleep 0.5
jq -n --argjson after "$(props $prefix-timer.service)" --argjson listed "$(listing $prefix-timer.service)" \
	--argjson journal "$(journal $prefix-timer.service "$c")" \
	'{timer: {after: $after, listed_after: $listed, journal: $journal}}' >"$tmp/timer.json"

jq -s 'add' "$tmp"/{stop,kill,crash,oneshot,timer}.json >"$out"
echo "wrote $out"
