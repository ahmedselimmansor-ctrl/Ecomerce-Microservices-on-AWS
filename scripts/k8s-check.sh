#!/usr/bin/env bash
# Renders both Kustomize overlays and asserts what every workload must satisfy.
#
# These are the rules that are cheap to state and expensive to discover in
# production. Kyverno enforces most of them in the cluster (infra/k8s/policies),
# but a policy that first fires at admission time fails a deploy at 2am; the
# same rule checked here fails a pull request instead.
#
# Every assertion below is something that has taken a real platform down:
#
#   :latest              two pods on the same "version" running different code
#   no memory limit      one leaky pod evicts its neighbours
#   no readiness probe   traffic routed to a pod still loading its JWKS
#   liveness == readiness  a database blip restarts every pod at once, turning
#                        a brownout into an outage
#   runAsRoot            a container escape starts with root in the namespace
#   dangling configMapKeyRef  the pod never starts, and the error is in events
#   probe path with no handler  the rollout hangs and nothing says why
#
# Usage:  scripts/k8s-check.sh
# Exit:   0 clean, 1 findings.

set -uo pipefail
cd "$(dirname "$0")/.."

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; DIM=$'\033[2m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
failures=0

if ! command -v kubectl >/dev/null 2>&1; then
  printf "  ${DIM}kubectl not installed; skipping${OFF}\n"
  exit 0
fi

for overlay in dev prod; do
  echo
  echo "${BOLD}=== overlay: $overlay ===${OFF}"

  rendered=$(kubectl kustomize "infra/k8s/overlays/$overlay" 2>&1)
  if [ $? -ne 0 ]; then
    echo "  ${RED}overlay does not render${OFF}"
    echo "$rendered" | head -5 | sed 's/^/    /'
    failures=$((failures + 1))
    continue
  fi

  echo "  ${GREEN}renders${OFF} ($(echo "$rendered" | grep -c '^kind:') objects)"

  OVERLAY="$overlay" RENDERED="$rendered" python3 - <<'PY'
import os, re, sys

rendered = os.environ["RENDERED"]
overlay = os.environ["OVERLAY"]

GREEN, RED, DIM, OFF = "\033[0;32m", "\033[0;31m", "\033[2m", "\033[0m"

docs = rendered.split("\n---\n")
problems = []


def field(block, pattern):
    m = re.search(pattern, block, re.M)
    return m.group(1) if m else None


# ---------------------------------------------------------------- ConfigMaps
declared = {}
for block in docs:
    if re.search(r"^kind: ConfigMap", block, re.M):
        name = field(block, r"^  name: (\S+)")
        if name:
            # Keys sit at two-space indent under `data:`.
            data = block.split("\ndata:\n", 1)
            keys = set(re.findall(r"^  ([\w.-]+):", data[1], re.M)) if len(data) > 1 else set()
            declared.setdefault(name, set()).update(keys)

secrets = {field(b, r"^  name: (\S+)") for b in docs
           if re.search(r"^kind: (Secret|ExternalSecret)", b, re.M)}

# --------------------------------------------------------------- Deployments
deployments = [b for b in docs if re.search(r"^kind: Deployment", b, re.M)]

for block in deployments:
    name = field(block, r"^  name: (\S+)") or "<unnamed>"

    # A tag that moves means two pods claiming the same version can run
    # different code, and a rollback has nothing to roll back to.
    for image in re.findall(r"^\s+image: (\S+)", block, re.M):
        if image.endswith(":latest") or ":" not in image.rsplit("/", 1)[-1]:
            problems.append(f"{name}: image '{image}' has no immutable tag")

    if "resources:" not in block:
        problems.append(f"{name}: declares no resource requests or limits")
    else:
        limits = block.split("limits:", 1)
        if len(limits) < 2 or "memory:" not in limits[1][:200]:
            # CPU limits are deliberately optional — throttling a latency-
            # sensitive pod is usually worse than letting it burst. Memory is
            # not: without a limit one leaky pod evicts its neighbours.
            problems.append(f"{name}: has no memory limit")

    if "readinessProbe:" not in block:
        problems.append(f"{name}: has no readiness probe")
    if "livenessProbe:" not in block:
        problems.append(f"{name}: has no liveness probe")

    # The probes must not be the same endpoint. A liveness probe that touches a
    # dependency turns a database blip into a simultaneous restart of every pod.
    live = re.search(r"livenessProbe:(.{0,400}?)(?=\n\s{10}\w|\Z)", block, re.S)
    ready = re.search(r"readinessProbe:(.{0,400}?)(?=\n\s{10}\w|\Z)", block, re.S)
    if live and ready:
        live_path = field(live.group(1), r"path: (\S+)")
        ready_path = field(ready.group(1), r"path: (\S+)")
        if live_path and live_path == ready_path:
            problems.append(
                f"{name}: liveness and readiness share '{live_path}' — a dependency "
                f"blip will restart every pod at once")

    if "runAsNonRoot: true" not in block:
        problems.append(f"{name}: does not set runAsNonRoot")
    if "readOnlyRootFilesystem: true" not in block:
        problems.append(f"{name}: does not set readOnlyRootFilesystem")
    if "allowPrivilegeEscalation: false" not in block:
        problems.append(f"{name}: does not set allowPrivilegeEscalation: false")

    # Every env reference must resolve, or the pod never starts and the reason
    # is only visible in `kubectl describe`.
    for cm, key in re.findall(
            r"configMapKeyRef:\s*\{?\s*name:\s*([\w-]+),?\s*key:\s*([\w.-]+)", block):
        if cm in declared and key not in declared[cm]:
            problems.append(f"{name}: configMapKeyRef {cm}/{key} is not declared")
        elif cm not in declared:
            problems.append(f"{name}: references ConfigMap '{cm}', which no manifest creates")

    for sec in re.findall(r"secretKeyRef:\s*\{?\s*name:\s*([\w-]+)", block):
        if sec not in secrets:
            problems.append(f"{name}: references Secret '{sec}', which no manifest creates")

# ------------------------------------------- probe paths that actually exist
# A probe pointing at a path no handler serves is a pod that never becomes
# ready, and the only symptom is a rollout that hangs. Checked for the Next.js
# apps because their routes are files on disk and can therefore be verified
# statically; the compiled services are checked by their own tests.
APP_ROOTS = {
    "storefront": "apps/storefront/src/app",
    "admin": "apps/admin/src/app",
}

for block in deployments:
    name = field(block, r"^  name: (\S+)") or ""
    root = APP_ROOTS.get(name)
    if not root:
        continue
    # Kustomize normalises flow style — `httpGet: { path: /x, port: http }` in
    # the source becomes a block mapping in the rendered output. Matching only
    # the flow form found nothing and the check silently passed, which is worse
    # than not having it: it reported success for work it never did.
    for path in set(re.findall(r"httpGet:\s*\n\s+path:\s*(\S+)", block)):
        handler = os.path.join(root, path.lstrip("/"), "route.ts")
        if not os.path.exists(handler):
            problems.append(f"{name}: probe {path} has no handler at {handler}")

# ------------------------------------------------- one PDB/HPA per Deployment
def names_of(kind):
    return {field(b, r"^  name: (\S+)") for b in docs
            if re.search(rf"^kind: {kind}", b, re.M)}

deployment_names = {field(b, r"^  name: (\S+)") for b in deployments}
pdbs = names_of("PodDisruptionBudget")
netpols = names_of("NetworkPolicy")

for d in sorted(n for n in deployment_names if n):
    if d not in pdbs:
        # Without one, a node drain can take every replica at once.
        problems.append(f"{d}: has no PodDisruptionBudget")
    if d not in netpols:
        problems.append(f"{d}: has no NetworkPolicy")

# --------------------------------------------------------------------- report
if problems:
    for p in problems:
        print(f"  {RED}FAIL{OFF}     {p}")
    print(f"\n  {RED}{len(problems)} problem(s) in the {overlay} overlay{OFF}")
    sys.exit(1)

checked_probes = sum(
    len(set(re.findall(r"httpGet:\s*\n\s+path:\s*(\S+)", b)))
    for b in deployments
    if APP_ROOTS.get(field(b, r"^  name: (\S+)") or "")
)

print(f"  {GREEN}policy{OFF}  {len(deployments)} deployments pass: immutable tag, memory limit,")
print(f"           {DIM}distinct probes, non-root, read-only rootfs, no privilege escalation,{OFF}")
print(f"           {DIM}every env reference resolves, PDB and NetworkPolicy present,{OFF}")
print(f"           {DIM}{checked_probes} Next.js probe paths resolve to a real route handler{OFF}")
PY
  [ $? -ne 0 ] && failures=$((failures + 1))
done

echo
if [ "$failures" -eq 0 ]; then
  echo "${GREEN}every workload policy held${OFF}"
else
  echo "${RED}$failures overlay(s) failed${OFF}"
  exit 1
fi
