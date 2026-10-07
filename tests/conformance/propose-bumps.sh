#!/usr/bin/env bash
# Opens (or updates) one pull request per upstream service that has a newer release.
#
#   tests/conformance/propose-bumps.sh [BUMPS_JSON]
#
# BUMPS_JSON is the output of `go run ./tests/conformance/bumpcheck -json FILE`; without it the
# script runs bumpcheck itself. For each slim-services artifact (and Studio) whose newest release
# is newer than the pin in internal/versions/versions.yaml it
#   - branches bump/<service> from the base branch, moves that one pin (deploy/releasetool bump),
#     commits and pushes;
#   - opens a pull request, or updates the open one when a still newer release has appeared since;
#   - starts ci.yml, conformance.yml and linux.yml (the conformance suite and the upgrade test) on the
#     branch, because a push made with the workflow's GITHUB_TOKEN does not start workflows by
#     itself.
# It never merges anything: a person reads the upstream release notes in the pull request and
# merges it once the runs are green. It skips a release that is a pre-release, a version whose
# pull request is already open, one whose pull request a person closed without merging, and a
# bump/<service> branch that carries a commit by anybody but the bot (a fix pushed to an open
# proposal); the force-push is leased to the commit it read, so a later push is never overwritten.
#
# Needs git, gh (GH_TOKEN with contents: write, pull-requests: write and actions: write),
# jq and either `go` or RELEASETOOL (a built deploy/releasetool). Environment: GITHUB_REPOSITORY,
# BASE_BRANCH (main), WORKFLOWS (the workflow files to start), DRY_RUN=1 to print what it would do
# and change nothing.
set -euo pipefail

repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}
base=${BASE_BRANCH:-main}
workflows=${WORKFLOWS:-"ci.yml conformance.yml linux.yml"}
dry=${DRY_RUN:-}
cd "$(git rev-parse --show-toplevel)"
versions=internal/versions/versions.yaml
bot_email="41898282+github-actions[bot]@users.noreply.github.com"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

tool() {
  if [[ -n ${RELEASETOOL:-} ]]; then "$RELEASETOOL" "$@"; else go run ./deploy/releasetool "$@"; fi
}
say() { echo "$*"; [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || echo "- $*" >>"$GITHUB_STEP_SUMMARY"; }
run() { if [[ -n $dry ]]; then echo "DRY RUN: $*"; else "$@"; fi; }

json=${1:-}
if [[ -z $json ]]; then
  json=$scratch/bumps.json
  # bumpcheck exits 1 when a pin is gone upstream; the rows are still written, and that failure
  # is bumpcheck's own job (conformance.yml) to report.
  go run ./tests/conformance/bumpcheck -json "$json" >/dev/null || true  # GITHUB_TOKEN, when set, raises its rate limit
fi
[[ -s $json ]] || { echo "no bumpcheck output at $json" >&2; exit 1; }

git fetch --quiet origin "$base"
base_ref=origin/$base
proposals=0

while IFS=$'\t' read -r svc pinned newest; do
  # The names come from upstream tag listings: only plain ones become a branch name.
  [[ $svc =~ ^[a-z][a-z0-9-]*$ && $newest =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]] || { say "$svc: skipped, the name or the tag has unexpected characters"; continue; }
  branch=bump/$svc
  title="Bump $svc to $newest"

  # What is already proposed? An open pull request for this version stands; a closed, unmerged one
  # means a person said no to it.
  existing=$(gh pr list --repo "$repo" --head "$branch" --state open --json number,title --jq '.[0] // empty')
  if [[ -n $existing && $(jq -r .title <<<"$existing") == "$title" ]]; then
    say "$svc: #$(jq -r .number <<<"$existing") already proposes $newest"
    continue
  fi
  declined=$(gh pr list --repo "$repo" --head "$branch" --state closed --json title,mergedAt |
    jq --arg t "$title" '[.[] | select(.title == $t and .mergedAt == null)] | length')
  if [[ $declined != 0 ]]; then
    say "$svc: $newest was proposed and closed without merging; not proposed again"
    continue
  fi

  # The branch belongs to this job, but a person may have pushed a fix to an open proposal. Read
  # where it stands now: the push below is leased to exactly that commit, and a branch that
  # carries a commit by anybody else is left alone.
  remote_sha=$(git ls-remote origin "refs/heads/$branch" | cut -f1)
  if [[ -n $remote_sha ]]; then
    git fetch --quiet origin "refs/heads/$branch"
    others=$(git log --format=%ae "$base_ref..$remote_sha" | grep -vxF "$bot_email" || true)
    if [[ -n $others ]]; then
      say "$svc: $branch has a commit that is not the bump job's; not touched (merge or close that pull request first)"
      continue
    fi
  fi

  git checkout --quiet -B "$branch" "$base_ref"
  rc=0
  tool bump -versions "$versions" -service "$svc" -to "$newest" || rc=$?
  if [[ $rc == 3 ]]; then
    say "$svc: $newest is a pre-release; not proposed"
    git checkout --quiet --detach "$base_ref"
    continue
  elif [[ $rc != 0 ]]; then
    git checkout --quiet --detach "$base_ref"
    echo "could not bump $svc to $newest" >&2
    exit "$rc"
  fi
  if git diff --quiet -- "$versions"; then
    say "$svc: $versions already pins $newest on $base"
    git checkout --quiet --detach "$base_ref"
    continue
  fi

  git show "$base_ref:$versions" >"$scratch/old.yaml"
  {
    echo "Moves the pin of **$svc** from \`$pinned\` to \`$newest\`."
    echo
    tool table -old "$scratch/old.yaml" -new "$versions"
    echo
    echo "Nothing merges this by itself. Before you merge it:"
    echo
    echo "- read the upstream release notes linked above, for every release between the two pins;"
    echo "- check that the runs below are green: **ci**, **conformance** (the supabase-js and CLI suites against a node) and **linux** (which holds the upgrade test). A push by a workflow does not start workflows, so the nightly proposal started them on this branch; the runs appear under *Checks* once they begin."
    if [[ $svc == studio ]]; then
      echo "- Studio is built from source with our patches (\`studio/patches\`): run the **studio** workflow on this branch, and update \`internal/config\` Regions if Studio's region list moved."
    fi
    echo
    echo "Opened by .github/workflows/bump-proposals.yml (tests/conformance/propose-bumps.sh)."
  } >"$scratch/body.md"

  GIT_AUTHOR_NAME="github-actions[bot]" GIT_AUTHOR_EMAIL="$bot_email" \
    GIT_COMMITTER_NAME="github-actions[bot]" GIT_COMMITTER_EMAIL="$bot_email" \
    git commit --quiet -m "$title" -m "Proposed by the nightly bump check. The conformance suite and the upgrade test gate the merge." -- "$versions"
  # The lease names the commit read above (empty: the branch must not exist yet), so a push that
  # landed since then makes this one fail instead of being overwritten.
  run git push "--force-with-lease=refs/heads/$branch:$remote_sha" origin "$branch"

  if [[ -n $existing ]]; then
    n=$(jq -r .number <<<"$existing")
    run gh pr edit "$n" --repo "$repo" --title "$title" --body-file "$scratch/body.md"
    say "$svc: updated #$n to $newest"
  else
    run gh pr create --repo "$repo" --base "$base" --head "$branch" --title "$title" --body-file "$scratch/body.md"
    say "$svc: opened a pull request for $newest"
  fi
  # A push with GITHUB_TOKEN starts no workflow; workflow_dispatch is the exception.
  for wf in $workflows $([[ $svc == studio ]] && echo studio.yml); do
    run gh workflow run "$wf" --repo "$repo" --ref "$branch"
  done
  proposals=$((proposals + 1))
  git checkout --quiet --detach "$base_ref"
done < <(jq -r '.[] | select(.install and .newer and .newest != "") | [.name, .pinned, .newest] | @tsv' "$json")

echo "$proposals proposal(s)"
