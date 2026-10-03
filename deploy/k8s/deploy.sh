#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
ENVIRONMENT=${ENVIRONMENT:-staging}
: "${ROOST_IMAGE:?ROOST_IMAGE must contain an immutable ghcr.io image digest}"
case "$ENVIRONMENT" in staging|production) ;; *) printf 'ENVIRONMENT must be staging or production\n' >&2; exit 2;; esac
case "$ROOST_IMAGE" in *@sha256:*) ;; *) printf 'ROOST_IMAGE must use an immutable digest\n' >&2; exit 2;; esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
cp -R "$ROOT/deploy/k8s" "$WORK/k8s"
cd "$WORK/k8s/overlays/$ENVIRONMENT"
kubectl kustomize . >/dev/null
if command -v kustomize >/dev/null 2>&1; then
  kustomize edit set image ghcr.io/CHANGE_ME/planet="$ROOST_IMAGE"
else
  printf 'kustomize CLI is required for an immutable image deployment\n' >&2
  exit 2
fi
kubectl kustomize . > "$WORK/rendered.yaml"
kubectl apply --server-side --dry-run=server -f "$WORK/rendered.yaml" >/dev/null
kubectl apply --server-side -f "$WORK/rendered.yaml"

kubectl -n roost get deployment,statefulset -l app.kubernetes.io/name=planet -o name | while IFS= read -r workload; do
  [ -z "$workload" ] || kubectl -n roost rollout status "$workload" --timeout=180s
done
printf 'deployed planet image %s to %s\n' "$ROOST_IMAGE" "$ENVIRONMENT"
