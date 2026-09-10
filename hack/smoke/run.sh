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
#   hack/smoke/run.sh                            # run all layers, tear down on exit
#   hack/smoke/run.sh --keep                     # leave fixtures in place for inspection
#   hack/smoke/run.sh --teardown                 # only tear down, do not run
#   hack/smoke/run.sh --with-isolation           # also run layer 6 (watch-namespaces isolation)
#   hack/smoke/run.sh --skip-layer-7             # skip layer 7 (templated pool ProviderID gate)
#   SMOKE_NS_ISOLATION=1 hack/smoke/run.sh       # same, via env
#   MOCK_IMAGE=... hack/smoke/run.sh             # override mock-redfish image
#   MOCK_INSPECTOR_IMAGE=... hack/smoke/run.sh   # override mock-inspector image
#
# Required: kubectl in PATH, current context with cert-manager + CAPI core
# installed and the beskar7 chart already deployed to capb7-system.
#
# Layer 6 (isolation) only does work when the operator was installed with
# --watch-namespaces (chart value watchNamespaces); otherwise it self-skips.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SCRIPT_DIR}/manifests"
SMOKE_NS="beskar7-smoke"
# Namespace used by the layer-6 isolation check. Deliberately NOT in the
# operator's --watch-namespaces list when running in watch-namespaces mode.
ISOLATION_NS="${SMOKE_NS}-unwatched"
OPERATOR_NS="${OPERATOR_NS:-capb7-system}"
OPERATOR_DEPLOY="${OPERATOR_DEPLOY:-capb7-controller-manager}"
MOCK_IMAGE="${MOCK_IMAGE:-}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"
# Grace window for the isolation check: how long to wait before asserting an
# out-of-scope PhysicalHost was NOT reconciled. A watching operator adds the
# finalizer on its first reconcile pass within ~1-2s, so 20s is ample.
ISOLATION_GRACE="${ISOLATION_GRACE:-20}"

KEEP_FIXTURES=0
TEARDOWN_ONLY=0
RUN_LAYER_2=1
RUN_LAYER_3=1
RUN_LAYER_4=1
RUN_LAYER_5=1
# Layer 6 (watch-namespaces isolation) is opt-in: only meaningful when the
# operator is installed with --watch-namespaces. Enable via --with-isolation
# or SMOKE_NS_ISOLATION=1. It self-skips if the operator watches all namespaces.
RUN_LAYER_6="${SMOKE_NS_ISOLATION:-0}"
# Layer 7 (templated pool / per-host ProviderID) is on by default: it guards
# D-014 P2, which is core provisioning behaviour rather than an optional topology.
RUN_LAYER_7=1

for arg in "$@"; do
  case "$arg" in
    --keep)            KEEP_FIXTURES=1 ;;
    --teardown)        TEARDOWN_ONLY=1 ;;
    --skip-layer-2)    RUN_LAYER_2=0 ;;
    --skip-layer-3)    RUN_LAYER_3=0 ;;
    --skip-layer-4)    RUN_LAYER_4=0 ;;
    --skip-layer-5)    RUN_LAYER_5=0 ;;
    --skip-layer-7)    RUN_LAYER_7=0 ;;
    --with-isolation)  RUN_LAYER_6=1 ;;
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

  # The layer-7 MachineDeployment/MachineSet go first: a MachineSet that
  # outlives its Machines recreates them (new finalizers, new claims against
  # hosts that are being deleted), and the namespace then takes minutes to
  # finalize instead of seconds.
  kubectl delete --ignore-not-found=true -n "${SMOKE_NS}" \
    machinedeployment,machineset --all --wait=false >/dev/null 2>&1 || true
  kubectl delete --ignore-not-found=true -n "${SMOKE_NS}" \
    machine,kubeadmconfig,beskar7machine,beskar7machinetemplate,beskar7cluster,cluster,physicalhost --all \
    --wait=false >/dev/null 2>&1 || true

  # Give controllers a few seconds to honour finalizers, then force-remove
  # any remaining finalizers so the namespace can actually finalize. CAPI
  # Cluster/Machine finalizers cancel cleanly; the beskar7 PhysicalHost
  # finalizer can hang if the Beskar7Machine claim was never fully released
  # (smoke test exit between layer 4a and layer 4b would leave this state).
  # Force-removing is safe for ephemeral smoke fixtures.
  sleep 5
  local obj name
  for obj in machinedeployment machineset machine kubeadmconfig beskar7machine beskar7cluster cluster physicalhost; do
    while read -r name; do
      [[ -z "${name}" ]] && continue
      kubectl -n "${SMOKE_NS}" patch "${name}" --type=merge \
        -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    done < <(kubectl -n "${SMOKE_NS}" get "${obj}" -o name 2>/dev/null || true)
  done

  kubectl delete --ignore-not-found=true namespace "${SMOKE_NS}" --wait=false >/dev/null 2>&1 || true

  # Layer-6 isolation fixtures (best-effort; namespace may not exist).
  if kubectl get ns "${ISOLATION_NS}" >/dev/null 2>&1; then
    while read -r name; do
      [[ -z "${name}" ]] && continue
      kubectl -n "${ISOLATION_NS}" patch "${name}" --type=merge \
        -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    done < <(kubectl -n "${ISOLATION_NS}" get physicalhost -o name 2>/dev/null || true)
    kubectl delete --ignore-not-found=true namespace "${ISOLATION_NS}" --wait=false >/dev/null 2>&1 || true
  fi
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
# Shared helpers: image resolution and BMC reachability
# ---------------------------------------------------------------------------

# resolve_component_image <component> <override> <override-var-name>
#
# Resolves the image for one of the smoke helper components, in priority order:
#   1. the override env var (forces imagePullPolicy=Always, because iterative
#      dev usually re-pushes the same tag)
#   2. auto-derived from the installed controller: same registry path with
#      "/beskar7" swapped for "/<component>", same tag. This keeps the helpers
#      in lockstep with the chart in use, so `make smoke` works against any
#      released version without manifest edits.
#   3. empty — leave the manifest's literal alone (a release-time default that
#      lags by one alpha when a fresh tag has just been cut).
#
# Writes RESOLVED_IMAGE and RESOLVED_PULL_POLICY (both possibly empty, meaning
# "keep what the manifest says"); render_component_manifest consumes them.
resolve_component_image() {
  local component="$1" override="$2" override_var="$3" controller_img
  RESOLVED_IMAGE=""
  RESOLVED_PULL_POLICY=""

  if [[ -n "${override}" ]]; then
    RESOLVED_IMAGE="${override}"
    RESOLVED_PULL_POLICY="Always"
    info "  ${component} image: ${RESOLVED_IMAGE} (from ${override_var})"
    return 0
  fi

  controller_img="$(kubectl -n "${OPERATOR_NS}" get deploy "${OPERATOR_DEPLOY}" \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  if [[ "${controller_img}" =~ ^(.+)/beskar7:(.+)$ ]]; then
    RESOLVED_IMAGE="${BASH_REMATCH[1]}/${component}:${BASH_REMATCH[2]}"
    info "  ${component} image: ${RESOLVED_IMAGE} (auto-derived from ${OPERATOR_DEPLOY})"
  else
    info "  ${component} image: <manifest default> (could not parse controller image '${controller_img}')"
  fi
}

# render_component_manifest <manifest> <component>
#
# Prints the manifest with the resolved image and pull policy substituted in,
# for piping straight into `kubectl apply -f -`.
#
# Substituting BEFORE the first apply is the point: patching the image after
# the fact (`kubectl set image`) starts a second rollout, and for the window
# between "the new pod is Ready" and "kube-proxy has moved the ClusterIP off
# the terminating pod" every connection to the Service is refused. A
# PhysicalHost created in that window fails its first BMC handshake. Jobs are
# immutable once their pod is scheduled, which makes a post-apply patch racy
# there for a second reason.
render_component_manifest() {
  local manifest="$1" component="$2"
  local script=""
  if [[ -n "${RESOLVED_IMAGE}" ]]; then
    script+="s|image: ghcr.io/projectbeskar/beskar7/${component}:.*|image: ${RESOLVED_IMAGE}|;"
  fi
  if [[ -n "${RESOLVED_PULL_POLICY}" ]]; then
    script+="s|imagePullPolicy: .*|imagePullPolicy: ${RESOLVED_PULL_POLICY}|;"
  fi
  if [[ -z "${script}" ]]; then
    cat "${manifest}"
    return 0
  fi
  sed "${script}" "${manifest}"
}

# wait_for_redfish_service <service> [timeout-seconds]
#
# Blocks until the mock BMC is actually reachable through its Service: an
# EndpointSlice carries a ready address, and a Redfish request to the service
# root comes back.
#
# `kubectl rollout status` is not that gate. It returns as soon as the pod is
# Ready, which is before the EndpointSlice is written and before kube-proxy
# has programmed the ClusterIP. A PhysicalHost created in that window gets
# `connection refused` on its first reconcile, parks in Error, and only clears
# on the controller's next retry — which is what made both E2E jobs fail on
# run 34458472589 while asserting a green "[PASS] ... state=Error".
#
# The request goes through the API server's service proxy rather than a curl
# pod: it needs no extra image (nothing to pull, nothing to rate-limit), it
# resolves the Service through the same ready-endpoint list, and it speaks
# HTTPS to the backend without verifying the mock's self-signed certificate.
# The Redfish service root is unauthenticated by spec, so no credentials are
# needed. If the caller lacks services/proxy RBAC the probe says so and the
# endpoint gate above stands on its own.
wait_for_redfish_service() {
  local svc="$1" timeout="${2:-120}"
  local deadline=$(( $(date +%s) + timeout ))
  local ready=0 out=""

  info "  waiting for Service ${svc} to have a ready endpoint"
  while [[ "$(date +%s)" -lt "${deadline}" ]]; do
    ready="$(kubectl -n "${SMOKE_NS}" get endpointslices \
      -l "kubernetes.io/service-name=${svc}" \
      -o jsonpath='{range .items[*].endpoints[*]}{.conditions.ready}{"\n"}{end}' 2>/dev/null \
      | grep -c '^true$' || true)"
    (( ready >= 1 )) && break
    sleep 2
  done
  if (( ready < 1 )); then
    fail "Service ${svc} has no ready endpoint after ${timeout}s"
    kubectl -n "${SMOKE_NS}" get endpointslices -l "kubernetes.io/service-name=${svc}" -o wide 2>&1 | sed 's/^/    /'
    kubectl -n "${SMOKE_NS}" describe deploy "${svc}" 2>&1 | tail -20 | sed 's/^/    /'
    return 1
  fi

  info "  probing the Redfish service root through Service ${svc}"
  while [[ "$(date +%s)" -lt "${deadline}" ]]; do
    if out="$(kubectl get --raw \
        "/api/v1/namespaces/${SMOKE_NS}/services/https:${svc}:8443/proxy/redfish/v1/" 2>&1)"; then
      if [[ "${out}" == *RedfishVersion* ]]; then
        pass "  ${svc} answers Redfish through its Service"
        return 0
      fi
      dim "  (unexpected service-root payload: $(printf '%s' "${out}" | head -c 120))"
    elif [[ "${out}" == *orbidden* ]]; then
      warn "  cannot probe ${svc} through the API server (no services/proxy permission); relying on the endpoint check"
      return 0
    fi
    sleep 2
  done

  fail "Redfish service root on ${svc} did not answer within ${timeout}s"
  printf '%s\n' "${out}" | head -5 | sed 's/^/    /'
  kubectl -n "${SMOKE_NS}" logs "deploy/${svc}" --tail=20 2>&1 | sed 's/^/    /'
  return 1
}

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
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
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

  resolve_component_image mock-redfish "${MOCK_IMAGE}" MOCK_IMAGE
  render_component_manifest "${MANIFEST_DIR}/10-mock-redfish.yaml" mock-redfish \
    | kubectl apply -f - >/dev/null

  kubectl apply -f "${MANIFEST_DIR}/20-bmc-secret.yaml" >/dev/null

  info "  waiting for mock-redfish pod to become ready"
  if ! kubectl -n "${SMOKE_NS}" rollout status deploy/mock-redfish --timeout=120s; then
    fail "[layer 3] mock-redfish failed to become ready"
    kubectl -n "${SMOKE_NS}" describe deploy/mock-redfish | tail -20
    return 1
  fi

  # Only now is the PhysicalHost worth creating: the host's first reconcile is
  # its BMC handshake, and a handshake against a Service with no programmed
  # endpoint fails.
  if ! wait_for_redfish_service mock-redfish; then
    fail "[layer 3] mock BMC never became reachable through its Service"
    return 1
  fi

  kubectl apply -f "${MANIFEST_DIR}/30-physicalhost.yaml" >/dev/null

  # The reconciler sets .status.ready (boolean) and .status.state; both must
  # line up. Ready=true with state=Error is a host that answered once and then
  # failed a later handshake, which is not a pass — the two fields are read in
  # one Get so they come from the same version of the object rather than from
  # two reads straddling a reconcile.
  info "  waiting up to ${WAIT_TIMEOUT} for PhysicalHost Ready=true, state=Available"
  local deadline=$(( $(date +%s) + ${WAIT_TIMEOUT%s} ))
  local observed=""
  while [[ "$(date +%s)" -lt "${deadline}" ]]; do
    observed="$(kubectl -n "${SMOKE_NS}" get physicalhost smoke-host-01 \
      -o jsonpath='{.status.ready}/{.status.state}' 2>/dev/null || true)"
    [[ "${observed}" == "true/Available" ]] && break
    sleep 3
  done
  if [[ "${observed}" != "true/Available" ]]; then
    fail "[layer 3] PhysicalHost did not reach Ready=true, state=Available in ${WAIT_TIMEOUT} (last seen: ${observed:-<empty>})"
    dim "  --- describe PhysicalHost ---"
    kubectl -n "${SMOKE_NS}" describe physicalhost smoke-host-01 | tail -40
    dim "  --- controller log (last 30) ---"
    kubectl -n "${OPERATOR_NS}" logs deploy/"${OPERATOR_DEPLOY}" --tail=30 | grep -iE "physicalhost|redfish|error" || true
    return 1
  fi
  pass "[layer 3] PhysicalHost Ready=true, state=Available"
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
  info "[layer 5] running mock-inspector Job to POST hardware report"

  resolve_component_image mock-inspector "${MOCK_INSPECTOR_IMAGE:-}" MOCK_INSPECTOR_IMAGE
  render_component_manifest "${MANIFEST_DIR}/50-mock-inspector-job.yaml" mock-inspector \
    | kubectl apply -f - >/dev/null

  # Wait for the Job to reach a terminal state. activeDeadlineSeconds=300
  # on the Job itself bounds the inner pod; --timeout here covers the
  # kubectl wait protocol overhead.
  info "  waiting up to ${WAIT_TIMEOUT} for mock-inspector Job to complete"
  if ! kubectl -n "${SMOKE_NS}" wait --for=condition=Complete \
        job/mock-inspector --timeout="${WAIT_TIMEOUT}" >/dev/null 2>&1; then
    # Either Failed condition or wait timeout. Dump pod logs either way.
    fail "[layer 5a] mock-inspector Job did not complete successfully"
    dim "--- Job status ---"
    kubectl -n "${SMOKE_NS}" get job mock-inspector -o yaml | tail -30
    dim "--- mock-inspector pod log ---"
    kubectl -n "${SMOKE_NS}" logs -l app.kubernetes.io/name=mock-inspector --tail=50 || true
    return 1
  fi
  pass "[layer 5a] mock-inspector Job completed (inspector POST accepted)"

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
# Layer 6: watch-namespaces isolation (opt-in)
#
# Verifies the SEC-2 watched-namespaces topology actually isolates: a
# PhysicalHost created in a namespace NOT in --watch-namespaces must be left
# completely untouched (no finalizer, no status) because the controller's
# informers are scoped away from it.
#
# Self-skips when the operator is in all-namespaces mode (no --watch-namespaces
# arg), so it is a no-op on the default smoke run.
# ---------------------------------------------------------------------------

layer_6_isolation() {
  info "[layer 6] verifying watch-namespaces isolation"

  # Read the operator's --watch-namespaces value, if any.
  local args watch_csv
  args="$(kubectl -n "${OPERATOR_NS}" get deploy "${OPERATOR_DEPLOY}" \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || echo '')"
  watch_csv="$(printf '%s' "${args}" | grep -oE '\-\-watch-namespaces=[^"]*' | head -1 | cut -d= -f2-)"

  if [[ -z "${watch_csv}" ]]; then
    warn "[layer 6] operator has no --watch-namespaces flag (all-namespaces mode); skipping isolation check"
    return 0
  fi
  info "    operator watches: ${watch_csv}"

  # Sanity: the smoke namespace must be watched (otherwise layers 3-5 couldn't
  # have passed), and the isolation namespace must NOT be.
  if [[ ",${watch_csv}," == *",${ISOLATION_NS},"* ]]; then
    fail "[layer 6] isolation namespace ${ISOLATION_NS} is in the watch list; test misconfigured"
    return 1
  fi

  info "    creating an out-of-scope PhysicalHost in ${ISOLATION_NS}"
  kubectl create namespace "${ISOLATION_NS}" >/dev/null 2>&1 || true
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHost
metadata:
  name: unwatched-host-01
  namespace: ${ISOLATION_NS}
spec:
  redfishConnection:
    address: "https://192.0.2.99"
    credentialsSecretRef: "no-such-secret"
    insecureSkipVerify: true
EOF

  info "    waiting ${ISOLATION_GRACE}s to confirm the controller ignores it"
  sleep "${ISOLATION_GRACE}"

  # A reconciling controller adds the finalizer on its first pass (before any
  # BMC I/O) and sets Status.State. Both must remain empty for an isolated host.
  local finalizers state
  finalizers="$(kubectl -n "${ISOLATION_NS}" get physicalhost unwatched-host-01 \
    -o jsonpath='{.metadata.finalizers}' 2>/dev/null || echo '')"
  state="$(kubectl -n "${ISOLATION_NS}" get physicalhost unwatched-host-01 \
    -o jsonpath='{.status.state}' 2>/dev/null || echo '')"

  if [[ -n "${finalizers}" && "${finalizers}" != "[]" ]]; then
    fail "[layer 6] out-of-scope PhysicalHost was reconciled (finalizers=${finalizers}); isolation broken"
    return 1
  fi
  if [[ -n "${state}" ]]; then
    fail "[layer 6] out-of-scope PhysicalHost got Status.State=${state}; isolation broken"
    return 1
  fi
  pass "[layer 6] out-of-scope PhysicalHost left untouched (no finalizer, no state)"
}


# ---------------------------------------------------------------------------
# Layer 7: templated multi-replica pool (D-014 P2)
#
# Proves the property a single hand-authored Machine cannot: that a SHARED
# Beskar7MachineTemplate yields a DISTINCT per-host ProviderID per replica.
# Before P2 this was the documented gap that blocked MachineDeployment pools
# and multi-replica control planes, so it is worth a standing CI gate rather
# than a one-off lab result.
#
# Asserts, for a replicas=2 MachineDeployment over two mock BMCs:
#   1. two Beskar7Machines are cloned from the one template
#   2. they claim two DIFFERENT PhysicalHosts
#   3. their ProviderIDs are DISTINCT   <- the regression P2 exists to prevent
#   4. each ProviderID is b7://<ns>/<its OWN claimed host>, not merely "some b7:// value"
# ---------------------------------------------------------------------------
layer_7_pool() {
  info "[layer 7] templated multi-replica pool: distinct per-host ProviderIDs"

  # The second mock BMC first, and reachable, before the PhysicalHost that
  # points at it exists: pool-host-b's first reconcile is its BMC handshake
  # (same ordering as layer 3).
  resolve_component_image mock-redfish "${MOCK_IMAGE}" MOCK_IMAGE
  render_component_manifest "${MANIFEST_DIR}/60-mock-redfish-b.yaml" mock-redfish \
    | kubectl apply -f - >/dev/null

  if ! kubectl -n "${SMOKE_NS}" rollout status deploy/mock-redfish-b --timeout=120s >/dev/null; then
    fail "[layer 7] second mock BMC failed to become ready"
    kubectl -n "${SMOKE_NS}" describe deploy/mock-redfish-b | tail -20
    return 1
  fi
  if ! wait_for_redfish_service mock-redfish-b; then
    fail "[layer 7] second mock BMC never became reachable through its Service"
    return 1
  fi

  kubectl apply -f "${MANIFEST_DIR}/61-pool.yaml" >/dev/null

  # Wait for the MachineSet to clone the template into two Beskar7Machines.
  local -i waited=0 count=0
  while (( waited < 120 )); do
    count="$(kubectl -n "${SMOKE_NS}" get beskar7machines \
      -l cluster.x-k8s.io/cluster-name=smoke-cluster,pool=smoke \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
    (( count >= 2 )) && break
    sleep 5; waited=$((waited+5))
  done
  if (( count < 2 )); then
    fail "[layer 7] MachineSet did not clone 2 Beskar7Machines from the template (saw ${count})"
    kubectl -n "${SMOKE_NS}" get machinedeployment,machineset,machine,beskar7machine 2>&1 | sed 's/^/    /'
    return 1
  fi
  pass "[layer 7] MachineSet cloned ${count} Beskar7Machines from one template"

  # Drive each host's inspection to completion, one explicit mock-inspector Job
  # per host (62-pool-inspectors.yaml). Both reuse the ServiceAccount and
  # namespace-scoped Role created by 50-mock-inspector-job.yaml in layer 5:
  # that Role already covers every host in the namespace.
  resolve_component_image mock-inspector "${MOCK_INSPECTOR_IMAGE:-}" MOCK_INSPECTOR_IMAGE
  render_component_manifest "${MANIFEST_DIR}/62-pool-inspectors.yaml" mock-inspector \
    | kubectl apply -f - >/dev/null

  local host
  for host in pool-host-a pool-host-b; do
    if ! kubectl -n "${SMOKE_NS}" wait --for=condition=complete \
         "job/mock-inspector-${host}" --timeout=300s >/dev/null 2>&1; then
      fail "[layer 7] mock-inspector Job for ${host} did not complete"
      kubectl -n "${SMOKE_NS}" logs "job/mock-inspector-${host}" --tail=40 2>&1 | sed 's/^/    /'
      return 1
    fi
  done
  pass "[layer 7] both hosts completed inspection"

  # Wait for both Beskar7Machines to carry a ProviderID.
  waited=0
  local ids=""
  while (( waited < 180 )); do
    ids="$(kubectl -n "${SMOKE_NS}" get beskar7machines \
      -l cluster.x-k8s.io/cluster-name=smoke-cluster,pool=smoke \
      -o jsonpath='{range .items[*]}{.metadata.name}={.spec.providerID}{"\n"}{end}' 2>/dev/null || true)"
    if [[ "$(printf '%s\n' "${ids}" | grep -c 'b7://' || true)" -ge 2 ]]; then break; fi
    sleep 5; waited=$((waited+5))
  done

  local -i with_id
  with_id="$(printf '%s\n' "${ids}" | grep -c 'b7://' || true)"
  if (( with_id < 2 )); then
    fail "[layer 7] fewer than 2 Beskar7Machines received a ProviderID"
    printf '%s\n' "${ids}" | sed 's/^/    /'
    return 1
  fi

  # (3) distinctness — the regression P2 exists to prevent.
  local -i uniq_ids
  uniq_ids="$(printf '%s\n' "${ids}" | grep 'b7://' | sed 's/.*=//' | sort -u | grep -c . || true)"
  if (( uniq_ids < 2 )); then
    fail "[layer 7] replicas share a ProviderID — the P2 regression"
    printf '%s\n' "${ids}" | sed 's/^/    /'
    return 1
  fi
  pass "[layer 7] ${uniq_ids} distinct ProviderIDs across the pool"

  # (4) each ProviderID must name the host that machine actually claimed.
  local line b7m id claimed_host
  while IFS= read -r line; do
    [[ "${line}" == *b7://* ]] || continue
    b7m="${line%%=*}"; id="${line#*=}"
    claimed_host="$(kubectl -n "${SMOKE_NS}" get physicalhosts \
      -o jsonpath="{range .items[?(@.spec.consumerRef.name==\"${b7m}\")]}{.metadata.name}{end}" 2>/dev/null || true)"
    if [[ -z "${claimed_host}" ]]; then
      fail "[layer 7] ${b7m} has ProviderID ${id} but claims no PhysicalHost"
      return 1
    fi
    if [[ "${id}" != "b7://${SMOKE_NS}/${claimed_host}" ]]; then
      fail "[layer 7] ${b7m} ProviderID ${id} does not match its own claimed host (expected b7://${SMOKE_NS}/${claimed_host})"
      return 1
    fi
    pass "[layer 7]   ${b7m} -> ${id} (claims ${claimed_host})"
  done <<< "${ids}"

  pass "[layer 7] templated pool yields correct per-host ProviderIDs"
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
[[ "${RUN_LAYER_6}" -eq 1 ]] && { layer_6_isolation  || FAILED=1; }
[[ "${RUN_LAYER_7}" -eq 1 ]] && { layer_7_pool       || FAILED=1; }

if [[ "${FAILED}" -eq 0 ]]; then
  pass "smoke test PASSED on context ${CONTEXT}"
  exit 0
fi
fail "smoke test FAILED on context ${CONTEXT}"
exit 1
