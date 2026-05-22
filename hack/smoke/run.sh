#!/usr/bin/env bash
#
# Layered smoke test for the Beskar7 controller against the current
# kubectl context. No bare-metal hardware required.
#
# Layers exercised:
#   1. Static install   - operator pod Running, CRDs present
#   2. Admission        - webhook rejects invalid PhysicalHost
#   3. Reconcile        - controller talks to mock BMC, PhysicalHost -> Available
#   4. CAPI claim       - Beskar7Machine claims the host, host state machine
#                         progresses (Available -> Inspecting/InUse)
#   5. Inspection       - simulate an iPXE-booted inspector POST to the
#                         controller's bootstrap callback; assert PhysicalHost
#                         reaches Ready and Beskar7Machine.Spec.ProviderID
#                         is set
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
RUN_LAYER_5=1

for arg in "$@"; do
  case "$arg" in
    --keep)         KEEP_FIXTURES=1 ;;
    --teardown)     TEARDOWN_ONLY=1 ;;
    --skip-layer-2) RUN_LAYER_2=0 ;;
    --skip-layer-3) RUN_LAYER_3=0 ;;
    --skip-layer-4) RUN_LAYER_4=0 ;;
    --skip-layer-5) RUN_LAYER_5=0 ;;
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

  # Drop CRs in dependency order so finalizers can release. Skip if the
  # namespace is already gone (idempotent re-runs).
  kubectl get ns "${SMOKE_NS}" >/dev/null 2>&1 || return 0

  kubectl delete --ignore-not-found=true -n "${SMOKE_NS}" \
    machine,kubeadmconfig,beskar7machine,beskar7cluster,cluster,physicalhost --all \
    --wait=false >/dev/null 2>&1 || true

  # Give controllers a few seconds to honour finalizers, then force-remove
  # any remaining finalizers so the namespace can actually finalize. CAPI
  # Cluster/Machine finalizers cancel cleanly; the beskar7 PhysicalHost
  # finalizer can hang if the Beskar7Machine claim was never fully released
  # (smoke test exit between layer 4a and layer 4b would leave this state).
  # Force-removing is safe for ephemeral smoke fixtures.
  sleep 5
  local obj name
  for obj in machine kubeadmconfig beskar7machine beskar7cluster cluster physicalhost; do
    while read -r name; do
      [[ -z "${name}" ]] && continue
      kubectl -n "${SMOKE_NS}" patch "${name}" --type=merge \
        -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    done < <(kubectl -n "${SMOKE_NS}" get "${obj}" -o name 2>/dev/null || true)
  done

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

  # Resolve the mock image. Priority:
  #   1. MOCK_IMAGE env var (operator override; forces imagePullPolicy=Always
  #      because iterative dev usually reuses the same tag).
  #   2. Auto-derive from the installed controller: same registry path with
  #      "/beskar7" swapped for "/mock-redfish". This keeps the mock in
  #      lockstep with the chart, so `make smoke` works against any released
  #      version without manifest edits.
  #   3. Fall back to the literal in the manifest (a release-time default
  #      that lags by one alpha when a fresh tag has just been cut).
  local controller_img mock_image="" pull_policy=""
  if [[ -n "${MOCK_IMAGE}" ]]; then
    mock_image="${MOCK_IMAGE}"
    pull_policy="Always"
    info "  mock image: ${mock_image} (from MOCK_IMAGE)"
  else
    controller_img="$(kubectl -n "${OPERATOR_NS}" get deploy "${OPERATOR_DEPLOY}" \
      -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
    if [[ "${controller_img}" =~ ^(.+)/beskar7:(.+)$ ]]; then
      mock_image="${BASH_REMATCH[1]}/mock-redfish:${BASH_REMATCH[2]}"
      info "  mock image: ${mock_image} (auto-derived from ${OPERATOR_DEPLOY})"
    else
      info "  mock image: <manifest default> (could not parse controller image '${controller_img}')"
    fi
  fi

  kubectl apply -f "${MANIFEST_DIR}/10-mock-redfish.yaml" >/dev/null
  if [[ -n "${mock_image}" ]]; then
    kubectl -n "${SMOKE_NS}" set image deploy/mock-redfish "mock-redfish=${mock_image}" >/dev/null
  fi
  if [[ -n "${pull_policy}" ]]; then
    kubectl -n "${SMOKE_NS}" patch deploy mock-redfish --type=json -p="[
      {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/imagePullPolicy\",\"value\":\"${pull_policy}\"}
    ]" >/dev/null
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

  # Layer 4b: the host progressed out of Available. Without a real inspector
  # POSTing to the bootstrap callback the state machine parks at "Inspecting"
  # (the controller's handleReadyHost - which sets ProviderID - only runs
  # once the host reaches StateReady). So we assert progression, not
  # ProviderID. Full ProviderID assertion belongs in a future layer that
  # spins up an inspector-simulator pod.
  info "  waiting up to ${WAIT_TIMEOUT} for PhysicalHost.Status.State to leave Available"
  local deadline2=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local state=""
  while [[ "$(date +%s)" -lt "${deadline2}" ]]; do
    state="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.state}' 2>/dev/null || true)"
    [[ -n "${state}" && "${state}" != "Available" ]] && break
    sleep 3
  done
  case "${state}" in
    Inspecting|InUse|Ready)
      pass "[layer 4b] PhysicalHost progressed to state=${state} (claim drove state machine)"
      ;;
    "")
      fail "[layer 4b] PhysicalHost state empty after ${WAIT_TIMEOUT}"
      return 1
      ;;
    Available)
      fail "[layer 4b] PhysicalHost stuck in state=Available after claim (controller did not progress)"
      dim "  --- describe Beskar7Machine ---"
      kubectl -n "${SMOKE_NS}" describe beskar7machine smoke-machine-01 | tail -30
      return 1
      ;;
    Error)
      fail "[layer 4b] PhysicalHost transitioned to state=Error"
      kubectl -n "${SMOKE_NS}" describe physicalhost smoke-host-01 | tail -20
      return 1
      ;;
    *)
      warn "[layer 4b] PhysicalHost in unexpected state=${state}"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# Layer 5: simulate an iPXE-booted inspector POSTing to the controller's
# bootstrap callback. Completes the Inspecting -> Ready transition that
# layer 4 stops short of, then asserts ProviderID gets set.
#
# Flow:
#   1. Wait for PhysicalHost.Status.Bootstrap.{URL,TokenHash} to be set.
#      The Beskar7Machine controller mints these once the host is claimed.
#   2. Read the plaintext token from Secret <hostName>-bootstrap-token
#      (key plaintext-token). The controller writes it there in lockstep
#      with publishing the hash to Status.Bootstrap.TokenHash.
#   3. Derive the inspection URL from the bootstrap URL: same host:port,
#      swap "/api/v1/bootstrap/" -> "/api/v1/inspection/".
#   4. POST a fake hardware report from inside the cluster (via a
#      one-shot kubectl run curl pod). The controller's callback server
#      lives at the in-cluster DNS name; -k is used because the cert
#      covers webhook-service, not controller-manager service.
#   5. Wait for PhysicalHost to reach state=Ready (controller picked up
#      the inspection-result annotation + ConfigMap).
#   6. Wait for Beskar7Machine.Spec.ProviderID to be set. Verify format
#      matches b7://<ns>/<host>.
# ---------------------------------------------------------------------------

layer_5_inspection() {
  info "[layer 5] simulating inspector POST to bootstrap callback"

  # 1+2. Bootstrap status + token Secret (with timeout).
  info "  waiting up to ${WAIT_TIMEOUT} for Bootstrap.URL + TokenHash"
  local deadline=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local bootstrap_url="" token_hash=""
  while [[ "$(date +%s)" -lt "${deadline}" ]]; do
    bootstrap_url="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.bootstrap.url}' 2>/dev/null || true)"
    token_hash="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.bootstrap.tokenHash}' 2>/dev/null || true)"
    if [[ -n "${bootstrap_url}" && -n "${token_hash}" ]]; then
      break
    fi
    sleep 3
  done
  if [[ -z "${bootstrap_url}" || -z "${token_hash}" ]]; then
    fail "[layer 5] Bootstrap.URL/TokenHash not populated within ${WAIT_TIMEOUT}"
    dim "  --- describe PhysicalHost ---"
    kubectl -n "${SMOKE_NS}" describe physicalhost smoke-host-01 | tail -30
    return 1
  fi
  dim "  bootstrap URL: ${bootstrap_url}"

  local token_b64
  token_b64="$(kubectl -n "${SMOKE_NS}" get secret smoke-host-01-bootstrap-token \
    -o jsonpath='{.data.plaintext-token}' 2>/dev/null || true)"
  if [[ -z "${token_b64}" ]]; then
    fail "[layer 5] bootstrap-token Secret missing plaintext-token key"
    return 1
  fi
  # Read the token + hash-cross-check with retry. The controller's mint
  # path has a small window where Secret.plaintext can lead the matching
  # hash into Status.Bootstrap.TokenHash by a few seconds (BootstrapToken
  # annotation in flight from Beskar7Machine to PhysicalHost reconciler).
  # The controller-side fix in beskar7machine_controller.go's
  # bootstrapTokenStillValid prevents the race from re-occurring on
  # subsequent reconciles, but a single retry here makes the runner
  # robust against the first observation. ~12s total budget; well under
  # the 600s inspection-timeout.
  local token computed match=0
  for attempt in 1 2 3 4 5 6; do
    token_b64="$(kubectl -n "${SMOKE_NS}" get secret smoke-host-01-bootstrap-token \
      -o jsonpath='{.data.plaintext-token}' 2>/dev/null || true)"
    token_hash="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.bootstrap.tokenHash}' 2>/dev/null || true)"
    if [[ -z "${token_b64}" || -z "${token_hash}" ]]; then
      sleep 2; continue
    fi
    token="$(printf '%s' "${token_b64}" | base64 -d)"
    if command -v sha256sum >/dev/null 2>&1; then
      computed="$(printf '%s' "${token}" | sha256sum | awk '{print $1}')"
      if [[ "${computed}" == "${token_hash}" ]]; then
        match=1
        dim "  token sha256 matches Status.TokenHash (prefix: ${computed:0:12}…, attempt ${attempt})"
        break
      fi
      dim "  attempt ${attempt}: sha256 mismatch (Secret: ${computed:0:8}…, Status: ${token_hash:0:8}…); retrying in 2s"
      sleep 2
    else
      # No sha256sum available — skip the cross-check, trust the values.
      match=1
      break
    fi
  done
  if [[ "${match}" -eq 0 ]]; then
    fail "[layer 5] token Secret never converged with Status.Bootstrap.TokenHash"
    dim "  Last Secret sha256: ${computed:0:12}…"
    dim "  Last Status.TokenHash: ${token_hash:0:12}…"
    return 1
  fi

  # 3. Derive inspection URL.
  local inspection_url="${bootstrap_url//\/api\/v1\/bootstrap\//\/api\/v1\/inspection\/}"
  if [[ "${inspection_url}" == "${bootstrap_url}" ]]; then
    fail "[layer 5] bootstrap URL did not contain /api/v1/bootstrap/ (cannot derive inspection URL): ${bootstrap_url}"
    return 1
  fi
  dim "  inspection URL: ${inspection_url}"

  # 4. POST a fake report from inside the cluster. Use a one-shot pod so we
  # do not need port-forward plumbing. -k skips TLS verify because the
  # cert-manager-issued cert covers beskar7-webhook-service, not the
  # controller-manager service the callback URL points at (real production
  # inspectors handle this either via a CA bundle in the OS image or by
  # accepting controller-manager.<ns>.svc as a SAN — out of scope here).
  local report
  report='{"manufacturer":"MockSmoke","model":"VirtualBox","serialNumber":"SMOKE-001","bootModeDetected":"UEFI","firmwareVersion":"1.0.0","cpus":[{"id":"cpu0","vendor":"GenuineIntel","model":"Mock CPU","cores":4,"threads":8,"frequency":"3.0GHz"}],"memory":[{"id":"DIMM0","type":"DDR4","capacity":"16GB","speed":"3200MHz"}],"disks":[{"name":"sda","model":"VirtualDisk","sizeGB":100,"type":"SSD","serialNumber":"SMOKE-DISK-001"}],"nics":[{"name":"eth0","macAddress":"de:ad:be:ef:00:01","driver":"virtio","speed":"1Gbps","ipAddresses":["10.0.0.42"]}]}'

  # kubectl run is idempotent on a clean namespace but will collide on
  # re-runs; --rm=true cleans up the pod even on success or failure.
  info "  POSTing fake hardware report via in-cluster curl pod"
  local post_out
  if ! post_out="$(kubectl -n "${SMOKE_NS}" run smoke-inspector \
        --image=curlimages/curl:8.10.1 \
        --restart=Never --rm=true --quiet --attach=true \
        --override-type=strategic \
        --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"runAsGroup":65532}}}' \
        --command -- \
        sh -c "curl -ksS -o - -w '\nHTTP_STATUS=%{http_code}\n' \
          -X POST '${inspection_url}' \
          -H 'Authorization: Bearer ${token}' \
          -H 'Content-Type: application/json' \
          --data-raw '${report}'" 2>&1)"; then
    fail "[layer 5] inspector POST pod failed"
    dim "  output: ${post_out}"
    return 1
  fi
  if ! grep -q 'HTTP_STATUS=2' <<<"${post_out}"; then
    fail "[layer 5] inspector POST returned non-2xx status"
    dim "  output: ${post_out}"
    return 1
  fi
  pass "[layer 5a] inspector POST accepted (2xx)"

  # 5. Host transitions to Ready.
  info "  waiting up to ${WAIT_TIMEOUT} for PhysicalHost.Status.State=Ready"
  local deadline2=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local state=""
  while [[ "$(date +%s)" -lt "${deadline2}" ]]; do
    state="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.state}' 2>/dev/null || true)"
    [[ "${state}" == "Ready" ]] && break
    sleep 3
  done
  if [[ "${state}" != "Ready" ]]; then
    fail "[layer 5b] PhysicalHost did not reach state=Ready (got: ${state}) within ${WAIT_TIMEOUT}"
    dim "  --- describe PhysicalHost ---"
    kubectl -n "${SMOKE_NS}" describe physicalhost smoke-host-01 | tail -30
    return 1
  fi
  pass "[layer 5b] PhysicalHost reached state=Ready"

  # 6. ProviderID set with expected format.
  info "  waiting up to ${WAIT_TIMEOUT} for Beskar7Machine.Spec.ProviderID"
  local deadline3=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local provider=""
  while [[ "$(date +%s)" -lt "${deadline3}" ]]; do
    provider="$(kubectl -n "${SMOKE_NS}" get beskar7machine smoke-machine-01 \
      -o jsonpath='{.spec.providerID}' 2>/dev/null || true)"
    [[ -n "${provider}" ]] && break
    sleep 3
  done
  if [[ -z "${provider}" ]]; then
    fail "[layer 5c] ProviderID was not set within ${WAIT_TIMEOUT}"
    dim "  --- describe Beskar7Machine ---"
    kubectl -n "${SMOKE_NS}" describe beskar7machine smoke-machine-01 | tail -30
    return 1
  fi
  local expected="b7://${SMOKE_NS}/smoke-host-01"
  if [[ "${provider}" != "${expected}" ]]; then
    fail "[layer 5c] ProviderID=${provider} != expected ${expected}"
    return 1
  fi
  pass "[layer 5c] Beskar7Machine ProviderID=${provider}"
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

declare -i FAILED=0

layer_1_static          || FAILED=1
[[ "${RUN_LAYER_2}" -eq 1 ]] && { layer_2_admission  || FAILED=1; }
[[ "${RUN_LAYER_3}" -eq 1 ]] && { layer_3_reconcile  || FAILED=1; }
[[ "${RUN_LAYER_4}" -eq 1 ]] && { layer_4_claim      || FAILED=1; }
[[ "${RUN_LAYER_5}" -eq 1 ]] && { layer_5_inspection || FAILED=1; }

if [[ "${FAILED}" -eq 0 ]]; then
  pass "smoke test PASSED on context ${CONTEXT}"
  exit 0
fi
fail "smoke test FAILED on context ${CONTEXT}"
exit 1
