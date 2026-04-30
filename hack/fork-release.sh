#!/usr/bin/env bash
# Build and push fork release images + Helm charts to Docker Hub under avionixg.
#
# Usage: hack/fork-release.sh [--skip-arm] [--skip-push] [--skip-build]
#
# --skip-arm    Single-arch amd64 release (no arm64 build, no manifest list).
# --skip-push   Build only.
# --skip-build  Push already-built images.
#
# Output layout (TAG = contents of VERSION file, e.g. v1.16.1):
#   avionixg/kube-ovn:TAG               multi-arch manifest (amd64+arm64)
#   avionixg/kube-ovn:TAG-amd64         single-arch image
#   avionixg/kube-ovn:TAG-arm64         single-arch image
#   avionixg/kube-ovn:TAG-debug         multi-arch manifest
#   avionixg/kube-ovn:TAG-debug-amd64   single-arch image
#   avionixg/kube-ovn:TAG-debug-arm64   single-arch image
#   avionixg/kube-ovn:TAG-amd64-legacy  single-arch image (CentOS/legacy base)
#   avionixg/vpc-nat-gateway:TAG        multi-arch manifest
#   avionixg/vpc-nat-gateway:TAG-amd64  single-arch image
#   avionixg/vpc-nat-gateway:TAG-arm64  single-arch image
#   avionixg/kube-ovn-chart:TAG         Helm chart (renamed from kube-ovn to avoid
#                                       clobbering the image manifest at the same tag)
#   avionixg/kube-ovn-v2:TAG            Helm chart
#
# arm64 builds use docker buildx + QEMU. If binfmt is not registered:
#   docker run --privileged --rm tonistiigi/binfmt --install arm64

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

REGISTRY=avionixg
TAG=$(cat VERSION)
DEBUG_TAG="${TAG}-debug"
LEGACY_TAG="${TAG}-amd64-legacy"

SKIP_ARM=0
SKIP_PUSH=0
SKIP_BUILD=0
for arg in "$@"; do
  case $arg in
    --skip-arm)   SKIP_ARM=1 ;;
    --skip-push)  SKIP_PUSH=1 ;;
    --skip-build) SKIP_BUILD=1 ;;
    *) echo "Unknown argument: $arg" >&2; exit 1 ;;
  esac
done

log() { echo "==> $*"; }

# Images that exist for both arches and get a manifest list.
MULTIARCH_IMAGES=(
  "kube-ovn:${TAG}"
  "kube-ovn:${DEBUG_TAG}"
  "vpc-nat-gateway:${TAG}"
)

# ---------- login (commented out; rely on existing docker login) ----------
if [[ $SKIP_PUSH -eq 0 ]]; then
  log "Assuming you are already logged in to Docker Hub as ${REGISTRY}"
  # docker login -u ${REGISTRY}
  # helm registry login registry-1.docker.io -u ${REGISTRY}
fi

# ---------- build amd64 ----------
# `make release` produces ${REGISTRY}/kube-ovn:${TAG}, :${DEBUG_TAG}, :${LEGACY_TAG}
# and ${REGISTRY}/vpc-nat-gateway:${TAG} -- all amd64. Retag immediately so the
# subsequent arm64 build doesn't overwrite them in the local image store.
if [[ $SKIP_BUILD -eq 0 ]]; then
  log "Building amd64 images"
  make release REGISTRY=$REGISTRY

  log "Retagging amd64 images with -amd64 suffix"
  for img in "${MULTIARCH_IMAGES[@]}"; do
    docker tag "${REGISTRY}/${img}" "${REGISTRY}/${img}-amd64"
  done
  # LEGACY_TAG is amd64-only by definition; already arch-suffixed in its name.
fi

# ---------- build arm64 ----------
if [[ $SKIP_BUILD -eq 0 && $SKIP_ARM -eq 0 ]]; then
  log "Building arm64 images (will overwrite amd64 tags locally; that's fine, we already retagged)"
  make release-arm REGISTRY=$REGISTRY

  log "Retagging arm64 images with -arm64 suffix"
  for img in "${MULTIARCH_IMAGES[@]}"; do
    docker tag "${REGISTRY}/${img}" "${REGISTRY}/${img}-arm64"
  done
fi

# ---------- push ----------
if [[ $SKIP_PUSH -eq 0 ]]; then
  log "Pushing amd64 images"
  for img in "${MULTIARCH_IMAGES[@]}"; do
    docker push "${REGISTRY}/${img}-amd64"
  done
  docker push "${REGISTRY}/kube-ovn:${LEGACY_TAG}"

  if [[ $SKIP_ARM -eq 0 ]]; then
    log "Pushing arm64 images"
    for img in "${MULTIARCH_IMAGES[@]}"; do
      docker push "${REGISTRY}/${img}-arm64"
    done

    log "Creating multi-arch manifest lists"
    for img in "${MULTIARCH_IMAGES[@]}"; do
      ref="${REGISTRY}/${img}"
      docker manifest rm "$ref" >/dev/null 2>&1 || true
      docker manifest create "$ref" "${ref}-amd64" "${ref}-arm64"
      docker manifest push "$ref"
    done
  else
    log "Single-arch release: tagging amd64 as canonical TAG"
    for img in "${MULTIARCH_IMAGES[@]}"; do
      docker tag "${REGISTRY}/${img}-amd64" "${REGISTRY}/${img}"
      docker push "${REGISTRY}/${img}"
    done
  fi

  # ---------- Helm charts ----------
  # Pushed to a separate repo name (kube-ovn-chart) so the chart tag does not
  # collide with the image manifest at avionixg/kube-ovn:${TAG}.
  log "Packaging and pushing Helm charts"
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  for chart in charts/kube-ovn charts/kube-ovn-v2; do
    helm package "$chart" --destination "$tmpdir"
  done
  for pkg in "$tmpdir"/*.tgz; do
    helm push "$pkg" "oci://registry-1.docker.io/${REGISTRY}"
  done
fi

log "Done. Published ${REGISTRY}/kube-ovn:${TAG} (multi-arch) and charts."
