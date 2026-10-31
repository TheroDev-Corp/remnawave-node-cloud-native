#!/bin/sh
# Run a Claude Code agent on this repo from the console, without GUI
# approval prompts. Permissions come from agent-settings.json next to this
# script (dev tools allowed; force-push, branch deletion, PR merge, sudo and
# hard resets denied; anything else not allowed is refused, never prompted).
#
#   agent.sh "task"          one-shot: runs the task, prints the result
#   agent.sh -c "follow-up"  continue the most recent run in this repo
#   agent.sh                 interactive session with the same permissions
#
# Uses claudep (claude behind the local proxy) when present, else claude.
# Override with CLAUDE_BIN=/path/to/claude.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
settings="$here/agent-settings.json"

bin=${CLAUDE_BIN:-}
if [ -z "$bin" ]; then
  if [ -x "$HOME/.local/bin/claudep" ]; then
    bin="$HOME/.local/bin/claudep"
  else
    bin=$(command -v claude || true)
  fi
fi
[ -n "$bin" ] || { echo "agent.sh: claude not found (set CLAUDE_BIN)" >&2; exit 1; }

cd "$repo"

if [ $# -eq 0 ]; then
  exec "$bin" --settings "$settings" --permission-mode dontAsk
fi

cont=""
if [ "$1" = "-c" ]; then
  cont="--continue"
  shift
fi
[ $# -gt 0 ] || { echo "agent.sh: missing task text" >&2; exit 2; }

exec "$bin" --settings "$settings" --permission-mode dontAsk $cont -p "$*"
