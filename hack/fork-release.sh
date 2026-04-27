#!/usr/bin/env bash
# Build and push release images + Helm charts to Docker Hub under avionixg.
#
# Usage: hack/fork-release.sh [--skip-arm] [--skip-push] [--skip-build]
#
# --skip-arm    Skip the arm64 build/push (single-arch amd64 release).
# --skip-push   Build only; do not push or publish anything.
# --skip-build  Skip docker build steps; only push/publish already-built images.
#
# Multi-arch notes:
#   amd64 builds natively. arm64 cross-compiles via docker buildx + QEMU.
#   If QEMU binfmt support is not registered on the host, arm64 builds will
#   fail; run: docker run --privileged --rm tonistiigi/binfmt --install arm64
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
    *) echo "Unknown argument: $arg"; exit 1 ;;
  esac
done

log() { echo "==> $*"; }

# ---------- login ----------
if [[ $SKIP_PUSH -eq 0 ]]; then
  log "Logging in to Docker Hub as avionixg"
  # docker login -u avionixg
  # helm registry login registry-1.docker.io -u avionixg
fi

# ---------- build ----------
if [[ $SKIP_BUILD -eq 0 ]]; then
  log "Building amd64 images (tag $TAG)"
  make release REGISTRY=$REGISTRY

  if [[ $SKIP_ARM -eq 0 ]]; then
    log "Building arm64 images (tag $TAG)"
    make release-arm REGISTRY=$REGISTRY
  fi
fi

# ---------- push images ----------
if [[ $SKIP_PUSH -eq 0 ]]; then
  log "Pushing amd64 images"
  docker tag ${REGISTRY}/kube-ovn:${TAG}        ${REGISTRY}/kube-ovn:${TAG}-x86
  docker tag ${REGISTRY}/kube-ovn:${LEGACY_TAG} ${REGISTRY}/kube-ovn:${LEGACY_TAG}
  docker tag ${REGISTRY}/kube-ovn:${DEBUG_TAG}  ${REGISTRY}/kube-ovn:${DEBUG_TAG}-x86
  docker tag ${REGISTRY}/vpc-nat-gateway:${TAG} ${REGISTRY}/vpc-nat-gateway:${TAG}-x86

  docker push ${REGISTRY}/kube-ovn:${TAG}-x86
  docker push ${REGISTRY}/kube-ovn:${LEGACY_TAG}
  docker push ${REGISTRY}/kube-ovn:${DEBUG_TAG}-x86
  docker push ${REGISTRY}/vpc-nat-gateway:${TAG}-x86

  if [[ $SKIP_ARM -eq 0 ]]; then
    log "Pushing arm64 images"
    docker tag ${REGISTRY}/kube-ovn:${TAG}        ${REGISTRY}/kube-ovn:${TAG}-arm
    docker tag ${REGISTRY}/kube-ovn:${DEBUG_TAG}  ${REGISTRY}/kube-ovn:${DEBUG_TAG}-arm
    docker tag ${REGISTRY}/vpc-nat-gateway:${TAG} ${REGISTRY}/vpc-nat-gateway:${TAG}-arm

    docker push ${REGISTRY}/kube-ovn:${TAG}-arm
    docker push ${REGISTRY}/kube-ovn:${DEBUG_TAG}-arm
    docker push ${REGISTRY}/vpc-nat-gateway:${TAG}-arm
  fi

  # ---------- multi-arch manifests ----------
  if [[ $SKIP_ARM -eq 0 ]]; then
    log "Creating multi-arch manifests"
    for img in \
      "${REGISTRY}/kube-ovn:${TAG}|${REGISTRY}/kube-ovn:${TAG}-x86|${REGISTRY}/kube-ovn:${TAG}-arm" \
      "${REGISTRY}/kube-ovn:${DEBUG_TAG}|${REGISTRY}/kube-ovn:${DEBUG_TAG}-x86|${REGISTRY}/kube-ovn:${DEBUG_TAG}-arm" \
      "${REGISTRY}/vpc-nat-gateway:${TAG}|${REGISTRY}/vpc-nat-gateway:${TAG}-x86|${REGISTRY}/vpc-nat-gateway:${TAG}-arm"
    do
      manifest="${img%%|*}"
      rest="${img#*|}"
      amd="${rest%%|*}"
      arm="${rest#*|}"
      set +e; docker manifest rm "$manifest" 2>/dev/null; set -e
      docker manifest create "$manifest" "$amd" "$arm"
      docker manifest push "$manifest"
    done
  else
    log "Tagging amd64-only manifest as canonical tag"
    docker push ${REGISTRY}/kube-ovn:${TAG}
    docker push ${REGISTRY}/kube-ovn:${DEBUG_TAG}
    docker push ${REGISTRY}/vpc-nat-gateway:${TAG}
  fi

  # ---------- Helm charts ----------
  log "Packaging and pushing Helm charts"
  tmpdir=$(mktemp -d)
  trap "rm -rf $tmpdir" EXIT

  for chart in charts/kube-ovn charts/kube-ovn-v2; do
    helm package "$chart" --destination "$tmpdir"
  done
  for pkg in "$tmpdir"/*.tgz; do
    helm push "$pkg" oci://registry-1.docker.io/${REGISTRY}
  done
fi

log "Done. Published ${REGISTRY}/kube-ovn:${TAG} and charts."
