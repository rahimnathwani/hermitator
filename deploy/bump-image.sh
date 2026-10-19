#!/bin/sh
# Pin the agent image to the newest Claude Code, pi and Playwright that
# have been out for a week.
#
# The image pins exact versions so a new release cannot change what
# hermitator parses without anyone noticing; this moves the pins forward
# in one step. Review the diff, then `make image` builds it on the server.
#
# A release younger than MIN_AGE_DAYS is passed over: a compromised one
# is usually caught and pulled within days. The same cutoff goes into the
# image as RELEASED_BEFORE, so the pins' own dependencies, which npm and
# uv resolve at build time, are no younger either.
set -eu
cd "$(dirname "$0")/.."
file=image/Containerfile
min_age_days=${MIN_AGE_DAYS:-7}
cutoff=$(node -e 'console.log(new Date(Date.now() - process.argv[1] * 864e5).toISOString())' "$min_age_days")

# settled PACKAGE [MINOR] prints the newest stable release of an npm
# package published before the cutoff, within MINOR ("1.64") if given.
settled() {
	npm view "$1" time --json | node -e '
		const [cutoff, minor] = process.argv.slice(1);
		let s = "";
		process.stdin.on("data", d => s += d).on("end", () => {
			const times = JSON.parse(s);
			const key = v => v.split(".").map(Number);
			const ok = Object.keys(times)
				.filter(v => /^\d+\.\d+\.\d+$/.test(v) && times[v] <= cutoff && (!minor || v.startsWith(minor + ".")))
				.sort((a, b) => { const x = key(a), y = key(b); return x[0] - y[0] || x[1] - y[1] || x[2] - y[2]; });
			if (!ok.length) { console.error("no release of the package before " + cutoff); process.exit(1); }
			console.log(ok.pop());
		});' "$cutoff" "${2:-}"
}

claude=$(settled @anthropic-ai/claude-code)
pi=$(settled @earendil-works/pi-coding-agent)
# The Python and Node Playwright must use the same browser build, which
# releases of one minor version share. Python releases trail Node's, so
# take the newest Python and the newest Node patch of its minor.
python=$(curl -fsS https://pypi.org/pypi/playwright/json | node -e '
	const cutoff = process.argv[1];
	let s = "";
	process.stdin.on("data", d => s += d).on("end", () => {
		const key = v => v.split(".").map(Number);
		const ok = Object.entries(JSON.parse(s).releases)
			.filter(([v, files]) => /^\d+\.\d+\.\d+$/.test(v) && files.length && files.every(f => f.upload_time_iso_8601 <= cutoff))
			.map(([v]) => v)
			.sort((a, b) => { const x = key(a), y = key(b); return x[0] - y[0] || x[1] - y[1] || x[2] - y[2]; });
		if (!ok.length) { console.error("no Python playwright before " + cutoff); process.exit(1); }
		console.log(ok.pop());
	});' "$cutoff")
minor=${python%.*}
node=$(settled playwright "$minor")

# later A B succeeds if version or timestamp A sorts after B.
later() {
	node -e '
		const key = v => /^\d+(\.\d+)*$/.test(v) ? v.split(".").map(Number) : [v];
		const [a, b] = process.argv.slice(1).map(key);
		for (let i = 0; i < Math.max(a.length, b.length); i++) {
			if ((a[i] ?? 0) !== (b[i] ?? 0)) process.exit((a[i] ?? 0) > (b[i] ?? 0) ? 0 : 1);
		}
		process.exit(1);' "$1" "$2"
}

# pin NAME VALUE moves an ARG forward; pins already past the week-old
# release stay, and so does the cutoff that lets them install.
pin() {
	old=$(sed -n "s/^ARG $1=//p" "$file")
	if [ "$old" = "$2" ]; then
		echo "$1: $2 (unchanged)"
	elif later "$old" "$2"; then
		echo "$1: $old (kept; newer than $2)"
	else
		sed -i.bak "s/^ARG $1=.*/ARG $1=$2/" "$file" && rm -f "$file.bak"
		echo "$1: $old -> $2"
	fi
}
pin CLAUDE_CODE_VERSION "$claude"
pin PI_VERSION "$pi"
pin PLAYWRIGHT_VERSION "$node"
pin PLAYWRIGHT_PYTHON_VERSION "$python"
pin RELEASED_BEFORE "$cutoff"
