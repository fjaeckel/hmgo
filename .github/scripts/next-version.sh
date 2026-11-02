#!/usr/bin/env bash
# Copyright (c) 2026 Frederic Jung
# SPDX-License-Identifier: BSD-3-Clause

# Works out the semantic version to release for a commit on the default
# branch, from the pull requests merged since the last release tag:
#
#   semver:major  →  X+1.0.0
#   semver:minor  →  X.Y+1.0
#   semver:patch  →  X.Y.Z+1   (also what a PR without a semver label gets)
#   semver:skip   →  the PR alone does not cause a release
#
# The highest bump among those pull requests wins, so a release that
# covers several merges (a queued workflow run was superseded, or a
# build failed) still gets the right version. Without any release tag
# the count starts at v0.0.0, i.e. the first release is v0.0.1 (or
# v0.1.0, v1.0.0). Commits pushed without a pull request do not cause a
# release. If the commit already carries a release tag (a re-run), that
# version is reported again.
#
# Usage: next-version.sh [commit]   (default HEAD; needs gh, jq and the
# full history with tags; GH_REPO=owner/repo, GH_TOKEN with read access
# to pull requests)
#
# Prints key=value lines (release, version, previous, bump, prs) and
# appends them to $GITHUB_OUTPUT when that is set.

set -euo pipefail

repo=${GH_REPO:?GH_REPO must be set to owner/repo}
base=${BASE_BRANCH:-master}
sha=$(git rev-parse "${1:-HEAD}^{commit}")
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'

emit() {
	local line
	for line in "$@"; do
		echo "$line"
		if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$line" >>"$GITHUB_OUTPUT"; fi
	done
}

# A re-run of an already released commit: report its version again.
tagged=$(git tag --points-at "$sha" | grep -E "$semver" | sort -V | tail -n1 || true)
if [ -n "$tagged" ]; then
	previous=$(git tag --merged "$sha^" 2>/dev/null | grep -E "$semver" | sort -V | tail -n1 || true)
	emit release=true "version=$tagged" "previous=$previous" bump=existing prs=
	exit 0
fi

previous=$(git tag --merged "$sha" | grep -E "$semver" | sort -V | tail -n1 || true)
if [ -n "$previous" ]; then
	range=("$previous..$sha")
else
	range=("$sha")
fi

# The pull requests merged into the base branch by the commits since
# the last release, each with its highest semver label (0 skip, 1
# patch, 2 minor, 3 major).
declare -A rank=()
commits=$(git rev-list --first-parent "${range[@]}")
for c in $commits; do
	# Assigned first, so that a failed API call stops the script
	# rather than silently leaving a pull request out.
	pulls=$(gh api "repos/$repo/commits/$c/pulls" --jq "
		.[] | select(.merged_at != null and .base.ref == \"$base\")
		| [.number, ([.labels[].name] as \$l
			| if any(\$l[]; . == \"semver:major\") then 3
			  elif any(\$l[]; . == \"semver:minor\") then 2
			  elif any(\$l[]; . == \"semver:patch\") then 1
			  elif any(\$l[]; . == \"semver:skip\") then 0
			  else 1 end)]
		| @tsv")
	while IFS=$'\t' read -r number r; do
		if [ -n "$number" ]; then rank[$number]=$r; fi
	done <<<"$pulls"
done

bump=0
prs=
for n in $(printf '%s\n' "${!rank[@]}" | sort -n); do
	prs+="${prs:+ }#$n"
	if [ "${rank[$n]}" -gt "$bump" ]; then bump=${rank[$n]}; fi
done

if [ "$bump" -eq 0 ]; then
	emit release=false version= "previous=$previous" bump=none "prs=$prs"
	exit 0
fi

IFS=. read -r major minor patch <<<"${previous:-v0.0.0}"
major=${major#v}
case $bump in
3) version="v$((major + 1)).0.0" name=major ;;
2) version="v$major.$((minor + 1)).0" name=minor ;;
1) version="v$major.$minor.$((patch + 1))" name=patch ;;
esac
emit release=true "version=$version" "previous=$previous" "bump=$name" "prs=$prs"
