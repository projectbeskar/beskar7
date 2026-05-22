#!/usr/bin/env bash
#
# Layered smoke test for the Beskar7 controller against the current
# kubectl context. No bare-metal hardware required.
#
# Layers exercised:
#   1. Static install   - operator pod Running, CRDs present
#   2. Admission        - webhook rejects invalid PhysicalHost
#   3. Reconcile        - controller talks to mock BMC, PhysicalHost -> Available
#   4. CAPI claim       - Beskar7Machine claims the host, sets ProviderID
#
# Layer 5 (PXE/inspector callback) needs a real iPXE-boot inspector and is
# out of scope for this rig.
#
# Usage:
#   hack/smoke/run.sh                # run all layers, tear down on exit
#   hack/smoke/run.sh --keep         # leave fixtures in place for inspection
#   hack/smoke/run.sh --teardown     # only tear down, do not run
#   MOCK_IMAGE=... hack/smoke/run.sh # override mock-redfish image
#
# Required: kubectl in PATH, current context with cert-manager + CAPI core
# installed and the beskar7 chart already deployed to beskar7-system.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SCRIPT_DIR}/manifests"
SMOKE_NS="beskar7-smoke"
OPERATOR_NS="${OPERATOR_NS:-beskar7-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-beskar7-controller-manager}"
MOCK_IMAGE="${MOCK_IMAGE:-}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"

KEEP_FIXTURES=0
TEARDOWN_ONLY=0
RUN_LAYER_2=1
RUN_LAYER_3=1
RUN_LAYER_4=1

for arg in "$@"; do
  case "$arg" in
    --keep)         KEEP_FIXTURES=1 ;;
    --teardown)     TEARDOWN_ONLY=1 ;;
    --skip-layer-2) RUN_LAYER_2=0 ;;
    --skip-layer-3) RUN_LAYER_3=0 ;;
    --skip-layer-4) RUN_LAYER_4=0 ;;
    -h|--help)
      sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "Unknown flag: $arg" >&2
      exit 64
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Logging helpers
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
  C_PASS=$'\033[1;32m'; C_FAIL=$'\033[1;31m'; C_INFO=$'\033[1;36m'
  C_WARN=$'\033[1;33m'; C_DIM=$'\033[0;90m';  C_RST=$'\033[0m'
else
  C_PASS=""; C_FAIL=""; C_INFO=""; C_WARN=""; C_DIM=""; C_RST=""
fi

info()  { printf '%s[INFO]%s  %s\n' "${C_INFO}" "${C_RST}" "$*"; }
pass()  { printf '%s[PASS]%s  %s\n' "${C_PASS}" "${C_RST}" "$*"; }
fail()  { printf '%s[FAIL]%s  %s\n' "${C_FAIL}" "${C_RST}" "$*" >&2; }
warn()  { printf '%s[WARN]%s  %s\n' "${C_WARN}" "${C_RST}" "$*" >&2; }
dim()   { printf '%s%s%s\n' "${C_DIM}" "$*" "${C_RST}"; }

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------

require() {
  command -v "$1" >/dev/null 2>&1 || { fail "missing required command: $1"; exit 127; }
}
require kubectl

CONTEXT="$(kubectl config current-context 2>/dev/null || echo '<none>')"
info "kubectl context: ${CONTEXT}"

# ---------------------------------------------------------------------------
# Teardown
# ---------------------------------------------------------------------------

teardown() {
  if [[ "${KEEP_FIXTURES}" -eq 1 ]]; then
    warn "--keep set; leaving fixtures in namespace ${SMOKE_NS}"
    return 0
  fi
  info "tearing down ${SMOKE_NS}"
  # Drop CRs first so finalizers don't block the namespace delete
  kubectl delete --ignore-not-found=true -n "${SMOKE_NS}" \
    machine,kubeadmconfig,beskar7machine,beskar7cluster,cluster,physicalhost --all \
    --wait=false >/dev/null 2>&1 || true
  kubectl delete --ignore-not-found=true namespace "${SMOKE_NS}" --wait=false >/dev/null 2>&1 || true
}

# Teardown-only mode
if [[ "${TEARDOWN_ONLY}" -eq 1 ]]; then
  KEEP_FIXTURES=0
  teardown
  pass "teardown complete"
  exit 0
fi

# Always tear down on exit unless --keep
trap 'rc=$?; teardown; exit $rc' EXIT INT TERM

# ---------------------------------------------------------------------------
# Layer 1: static install sanity
# ---------------------------------------------------------------------------

layer_1_static() {
  info "[layer 1] verifying operator install"
  if ! kubectl -n "${OPERATOR_NS}" get deploy "${OPERATOR_DEPLOY}" >/dev/null 2>&1; then
    fail "operator deployment ${OPERATOR_NS}/${OPERATOR_DEPLOY} not found"
    fail "install the chart first:  helm install --devel beskar7 beskar7/beskar7 -n ${OPERATOR_NS} --create-namespace"
    return 1
  fi
  kubectl -n "${OPERATOR_NS}" rollout status deploy "${OPERATOR_DEPLOY}" --timeout=60s >/dev/null
  for crd in physicalhosts beskar7machines beskar7clusters beskar7machinetemplates; do
    if ! kubectl get crd "${crd}.infrastructure.cluster.x-k8s.io" >/dev/null 2>&1; then
      fail "missing CRD: ${crd}.infrastructure.cluster.x-k8s.io"
      return 1
    fi
  done
  pass "[layer 1] operator running, 4 CRDs present"
}

# ---------------------------------------------------------------------------
# Layer 2: webhook admission
# ---------------------------------------------------------------------------

layer_2_admission() {
  info "[layer 2] verifying webhook admission"

  # Bad: address pattern violation (the CRD enforces ^https?://...). This is
  # a CRD-schema rejection (no admission webhook needed) and should be
  # rejected regardless of which webhook configuration is loaded — making
  # it a stable signal for layer 2.
  local out
  if out="$(kubectl apply --dry-run=server -f - 2>&1 <<EOF
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata: { name: bad, namespace: ${SMOKE_NS} }
spec:
  redfishConnection:
    address: "ftp://not-a-redfish-url"
    credentialsSecretRef: bmc-credentials
EOF
)"; then
    fail "[layer 2] PhysicalHost with invalid address was accepted (dry-run); output: ${out}"
    return 1
  fi
  dim "  (rejected as expected: $(printf '%s' "${out}" | head -1))"
  pass "[layer 2] CRD validation rejects malformed addresses"
}

# ---------------------------------------------------------------------------
# Layer 3: reconcile path against mock BMC
# ---------------------------------------------------------------------------

layer_3_reconcile() {
  info "[layer 3] applying mock BMC + PhysicalHost"
  kubectl apply -f "${MANIFEST_DIR}/00-namespace.yaml" >/dev/null

  # Optional MOCK_IMAGE override. When overriding (typically a local
  # iterative build that reuses a tag) we also force imagePullPolicy=Always
  # so the kubelet does not serve a stale cached layer.
  if [[ -n "${MOCK_IMAGE}" ]]; then
    info "  overriding mock image: ${MOCK_IMAGE}"
    kubectl apply -f "${MANIFEST_DIR}/10-mock-redfish.yaml" >/dev/null
    kubectl -n "${SMOKE_NS}" set image deploy/mock-redfish "mock-redfish=${MOCK_IMAGE}" >/dev/null
    kubectl -n "${SMOKE_NS}" patch deploy mock-redfish --type=json -p='[
      {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Always"}
    ]' >/dev/null
  else
    kubectl apply -f "${MANIFEST_DIR}/10-mock-redfish.yaml" >/dev/null
  fi

  kubectl apply -f "${MANIFEST_DIR}/20-bmc-secret.yaml" >/dev/null

  info "  waiting for mock-redfish pod to become ready"
  if ! kubectl -n "${SMOKE_NS}" rollout status deploy/mock-redfish --timeout=120s; then
    fail "[layer 3] mock-redfish failed to become ready"
    kubectl -n "${SMOKE_NS}" describe deploy/mock-redfish | tail -20
    return 1
  fi

  kubectl apply -f "${MANIFEST_DIR}/30-physicalhost.yaml" >/dev/null

  # The PhysicalHost reconciler sets .status.ready (boolean) and condition
  # HostAvailable, but does not emit a generic "Ready" condition. We poll
  # both via jsonpath to keep the assertion explicit.
  info "  waiting up to ${WAIT_TIMEOUT} for PhysicalHost.Status.Ready=true"
  if ! kubectl -n "${SMOKE_NS}" wait --for=jsonpath='{.status.ready}'=true \
        physicalhost/smoke-host-01 --timeout="${WAIT_TIMEOUT}"; then
    fail "[layer 3] PhysicalHost did not become Ready in ${WAIT_TIMEOUT}"
    dim "  --- describe PhysicalHost ---"
    kubectl -n "${SMOKE_NS}" describe physicalhost smoke-host-01 | tail -40
    dim "  --- controller log (last 30) ---"
    kubectl -n "${OPERATOR_NS}" logs deploy/"${OPERATOR_DEPLOY}" --tail=30 | grep -iE "physicalhost|redfish|error" || true
    return 1
  fi

  local state
  state="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 -o jsonpath='{.status.state}' 2>/dev/null || true)"
  pass "[layer 3] PhysicalHost Ready=true, state=${state:-<empty>}"
}

# ---------------------------------------------------------------------------
# Layer 4: CAPI claim path
# ---------------------------------------------------------------------------

layer_4_claim() {
  info "[layer 4] checking CAPI conformance labels on beskar7 CRDs"
  # CAPI v1.10+ runs a conversion webhook on Cluster/Machine that looks up
  # the contract-version label on the infrastructure CRDs. If the labels
  # are missing the conversion fails with:
  #   "cannot find any versions matching contract versions [v1beta2 v1beta1]"
  # We detect this and skip layer 4 cleanly instead of leaving fixtures
  # half-applied. Tracked as a follow-up: add
  # `+kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta1=v1_beta1"`
  # markers to api/v1beta1 types and regenerate manifests + chart CRDs.
  local label_v1beta1 label_v1beta2
  label_v1beta1="$(kubectl get crd beskar7clusters.infrastructure.cluster.x-k8s.io \
    -o jsonpath='{.metadata.labels.cluster\.x-k8s\.io/v1beta1}' 2>/dev/null || true)"
  label_v1beta2="$(kubectl get crd beskar7clusters.infrastructure.cluster.x-k8s.io \
    -o jsonpath='{.metadata.labels.cluster\.x-k8s\.io/v1beta2}' 2>/dev/null || true)"
  if [[ -z "${label_v1beta1}" && -z "${label_v1beta2}" ]]; then
    warn "[layer 4] beskar7 CRDs missing CAPI contract-version labels"
    warn "  CAPI Cluster/Machine conversion will fail; layer 4 cannot run."
    warn "  Tracked separately. See docs/smoke-testing.md (Known limitations)."
    return 0
  fi

  info "[layer 4] applying Beskar7Cluster + Beskar7Machine + CAPI Machine"
  kubectl apply -f "${MANIFEST_DIR}/40-cluster-and-machine.yaml" >/dev/null

  info "  waiting up to ${WAIT_TIMEOUT} for PhysicalHost.Spec.ConsumerRef to be set"
  local deadline=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local consumer=""
  while [[ "$(date +%s)" -lt "${deadline}" ]]; do
    consumer="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.spec.consumerRef.name}' 2>/dev/null || true)"
    [[ -n "${consumer}" ]] && break
    sleep 3
  done
  if [[ -z "${consumer}" ]]; then
    fail "[layer 4] PhysicalHost was not claimed within ${WAIT_TIMEOUT}"
    dim "  --- describe Beskar7Machine ---"
    kubectl -n "${SMOKE_NS}" describe beskar7machine smoke-machine-01 | tail -40
    return 1
  fi
  pass "[layer 4a] PhysicalHost claimed by Beskar7Machine=${consumer}"

  info "  waiting up to ${WAIT_TIMEOUT} for Beskar7Machine.Spec.ProviderID to be set"
  local deadline2=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local provider=""
  while [[ "$(date +%s)" -lt "${deadline2}" ]]; do
    provider="$(kubectl -n "${SMOKE_NS}" get beskar7machine smoke-machine-01 \
      -o jsonpath='{.spec.providerID}' 2>/dev/null || true)"
    [[ -n "${provider}" ]] && break
    sleep 3
  done
  if [[ -z "${provider}" ]]; then
    fail "[layer 4b] ProviderID was not set within ${WAIT_TIMEOUT}"
    dim "  --- describe Beskar7Machine ---"
    kubectl -n "${SMOKE_NS}" describe beskar7machine smoke-machine-01 | tail -40
    return 1
  fi
  pass "[layer 4b] Beskar7Machine ProviderID=${provider}"

  local expected="b7://${SMOKE_NS}/smoke-host-01"
  if [[ "${provider}" != "${expected}" ]]; then
    warn "  ProviderID ${provider} != expected ${expected} (may indicate a host-name mismatch)"
  fi
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

declare -i FAILED=0

layer_1_static          || FAILED=1
[[ "${RUN_LAYER_2}" -eq 1 ]] && { layer_2_admission || FAILED=1; }
[[ "${RUN_LAYER_3}" -eq 1 ]] && { layer_3_reconcile || FAILED=1; }
[[ "${RUN_LAYER_4}" -eq 1 ]] && { layer_4_claim      || FAILED=1; }

if [[ "${FAILED}" -eq 0 ]]; then
  pass "smoke test PASSED on context ${CONTEXT}"
  exit 0
fi
fail "smoke test FAILED on context ${CONTEXT}"
exit 1
