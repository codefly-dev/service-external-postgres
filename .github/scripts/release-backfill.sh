#!/usr/bin/env bash

set -euo pipefail

validate_tag() {
  local source_dir=$1
  local tag_ref="refs/tags/$RELEASE_TAG"

  git check-ref-format "$tag_ref" >/dev/null
  git -C "$source_dir" show-ref --verify --quiet "$tag_ref"

  local head_commit
  local tag_commit
  head_commit=$(git -C "$source_dir" rev-parse --verify 'HEAD^{commit}')
  tag_commit=$(git -C "$source_dir" rev-parse --verify "${tag_ref}^{commit}")
  if [[ "$head_commit" != "$tag_commit" ]]; then
    echo "$RELEASE_TAG does not point at the checked-out commit" >&2
    return 1
  fi

  printf 'GORELEASER_CURRENT_TAG=%s\n' "$RELEASE_TAG" >>"$GITHUB_ENV"
}

publish_release() {
  local dist_dir=$1
  local changelog="$dist_dir/CHANGELOG.md"
  local release_pages
  local release

  shopt -s nullglob
  local assets=("$dist_dir"/*.tar.gz "$dist_dir"/*_checksums.txt)
  shopt -u nullglob

  if ((${#assets[@]} == 0)); then
    echo "no release assets found in $dist_dir" >&2
    return 1
  fi
  if [[ ! -f "$changelog" ]]; then
    echo "release changelog not found at $changelog" >&2
    return 1
  fi

  release_pages=$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases?per_page=100")
  release=$(jq -c --arg tag "$RELEASE_TAG" \
    '[.[][] | select(.tag_name == $tag)][0] // empty' <<<"$release_pages")

  if [[ -z "$release" ]]; then
    gh release create "$RELEASE_TAG" \
      --repo "$GITHUB_REPOSITORY" \
      --verify-tag \
      --latest=false \
      --title "$RELEASE_TAG" \
      --notes-file "$changelog" \
      "${assets[@]}"
    return
  fi

  local missing_assets=()
  local asset
  for asset in "${assets[@]}"; do
    if ! jq -e --arg name "${asset##*/}" \
      '.assets[]? | select(.name == $name)' <<<"$release" >/dev/null; then
      missing_assets+=("$asset")
    fi
  done

  if ((${#missing_assets[@]} > 0)); then
    gh release upload "$RELEASE_TAG" "${missing_assets[@]}" \
      --repo "$GITHUB_REPOSITORY"
  fi

  if [[ "$(jq -r '.draft' <<<"$release")" == "true" ]]; then
    gh release edit "$RELEASE_TAG" \
      --repo "$GITHUB_REPOSITORY" \
      --draft=false \
      --latest=false
  fi
}

: "${RELEASE_TAG:?RELEASE_TAG is required}"

case "${1:-}" in
validate-tag)
  : "${GITHUB_ENV:?GITHUB_ENV is required}"
  validate_tag "${2:?source directory is required}"
  ;;
publish)
  : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
  : "${GH_TOKEN:?GH_TOKEN is required}"
  publish_release "${2:?distribution directory is required}"
  ;;
*)
  echo "usage: $0 {validate-tag SOURCE_DIR|publish DIST_DIR}" >&2
  exit 2
  ;;
esac
