#!/bin/sh
# Pin the agent image to the newest Claude Code, pi and Playwright.
#
# The image pins exact versions so a new release cannot change what
# hermitator parses without anyone noticing; this moves the pins forward
# in one step. Review the diff, then `make image` builds it on the server.
set -eu
cd "$(dirname "$0")/.."
file=image/Containerfile

latest() { npm view "$1" version; }
claude=$(latest @anthropic-ai/claude-code)
pi=$(latest @earendil-works/pi-coding-agent)
# The Python and Node Playwright must use the same browser build, which
# releases of one minor version share. Python releases trail Node's, so
# take the newest Python and the newest Node patch of its minor.
python=$(curl -fsS https://pypi.org/pypi/playwright/json |
	node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).info.version))')
minor=${python%.*}
node=$(npm view "playwright@~$minor.0" version --json |
	node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const v=JSON.parse(s);console.log([].concat(v).pop())})')

pin() {
	old=$(sed -n "s/^ARG $1=//p" "$file")
	if [ "$old" != "$2" ]; then
		sed -i.bak "s/^ARG $1=.*/ARG $1=$2/" "$file" && rm -f "$file.bak"
		echo "$1: $old -> $2"
	else
		echo "$1: $2 (unchanged)"
	fi
}
pin CLAUDE_CODE_VERSION "$claude"
pin PI_VERSION "$pi"
pin PLAYWRIGHT_VERSION "$node"
pin PLAYWRIGHT_PYTHON_VERSION "$python"
