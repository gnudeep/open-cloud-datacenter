#!/usr/bin/env bash
# repave-e2e.sh — stage-based e2e test runner for DBaaS OS repave + teardown.
# See P012-repave-testing-guide.md for the test matrix (T1..T10).
#
# Usage:
#   NS=tenant-acme ID=test-img-deletion YAML=./test.yaml ./repave-e2e.sh stage1
#   NS=tenant-acme ID=test-img-deletion                  ./repave-e2e.sh stage2
#   NS=tenant-acme ID=test-img-deletion YAML=./test.yaml ./repave-e2e.sh stage3
#   NS=tenant-acme ID=test-img-deletion                  ./repave-e2e.sh stage4
#   NS=... ID=... YAML=... ./repave-e2e.sh all     # pauses at controller redeploy
#
# IMPORTANT: ID must exactly match metadata.name in YAML — the script
# validates this up front and aborts on mismatch (every assertion keys off ID).
#
# stage4 (PG major-version EOL, E1-E3): run against an EXISTING Available
# instance (no YAML needed — it stays untouched throughout, that's the point).
# Precondition, done by hand before invoking: publish a new baked image
# revision whose PGVersions drops the instance's engineVersion, point
# LatestBakedImages[osStream] at it with Validated: true, and `make deploy`.
# Not part of `all` — it needs a distinct catalog bump from the plain
# OS-only bump `all` uses, and it deliberately does not delete/recreate $ID
# (stage3 does), so it must run before stage3 against the same instance.
#
# Env:
#   NS       instance namespace                  (required)
#   ID       DBInstance name — MUST match YAML's metadata.name (required)
#   YAML     path to the DBInstance manifest     (stage1/stage3/all)
#   DBNAME   database name for psql checks       (default: auto-read from the
#            live DBInstance's spec.dbName; the controller itself only falls
#            back to the instance name when spec.dbName is unset — see the
#            dbName default in dbinstance_controller.go — so a YAML with an
#            explicit dbName: like "orders" is honored automatically)
#   PGUSER   master username                     (default: auto-read from the
#            credentials Secret's admin_user)
#   SKIP_DB=1  skip psql-based data checks (no VLAN reach from this host)
#   TIMEOUT  seconds to wait for phase changes   (default: 900)
#
# Results: every run tees full output to results/<ns>.<id>.log (next to this
# script) and appends one row to results/summary.tsv — timestamp, ns, id,
# stage, pass count, fail count, overall result. Useful for showing a
# multi-run history without digging through terminal scrollback.
set -uo pipefail

case "${1:-}" in stage1|stage2|stage3|stage4|all) ;; *)
  echo "usage: NS=<ns> ID=<name> [YAML=<file>] [SKIP_DB=1] $0 {stage1|stage2|stage3|stage4|all}"; exit 2 ;;
esac

STAGE_ARG="$1"
NS="${NS:?set NS}"; ID="${ID:?set ID}"
# DBNAME and PGUSER intentionally left unset here — db_exec auto-resolves
# both from the live DBInstance / its credentials Secret unless the caller
# explicitly overrides them (see resolve_dbname and db_exec below).
TIMEOUT="${TIMEOUT:-900}"
STATE="/tmp/repave-e2e.${NS}.${ID}.state"
ANNOT="dbaas.opencloud.wso2.com/repave-trigger=now"
FAILED=0
PASS_COUNT=0
FAIL_COUNT=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="$SCRIPT_DIR/results"
mkdir -p "$RESULTS_DIR"
LOG="$RESULTS_DIR/${NS}.${ID}.log"
SUMMARY="$RESULTS_DIR/summary.tsv"
[ -f "$SUMMARY" ] || printf 'timestamp\tns\tid\tstage\tpass\tfail\tresult\n' > "$SUMMARY"
{ echo; echo "=== $(date -u +%Y-%m-%dT%H:%M:%SZ) :: $STAGE_ARG ==="; } >> "$LOG"
exec > >(tee -a "$LOG") 2>&1

# ---------- helpers ----------
say()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
pass() { printf '\033[1;32mPASS\033[0m  %s\n' "$*"; PASS_COUNT=$((PASS_COUNT+1)); }
fail() { printf '\033[1;31mFAIL\033[0m  %s\n' "$*"; FAILED=1; FAIL_COUNT=$((FAIL_COUNT+1)); }
die()  { printf '\033[1;31mABORT\033[0m %s\n' "$*"; record_summary "ABORT"; exit 1; }

record_summary() { # record_summary <result>
  printf '%s\t%s\t%s\t%s\t%d\t%d\t%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$NS" "$ID" "$STAGE_ARG" "$PASS_COUNT" "$FAIL_COUNT" "$1" >> "$SUMMARY"
}

phase() { kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.provisioningPhase}' 2>/dev/null; }

wait_phase() { # wait_phase <target> [timeout]
  local target="$1" t="${2:-$TIMEOUT}" start now p
  start=$(date +%s)
  while true; do
    p=$(phase)
    [ "$p" = "$target" ] && return 0
    now=$(date +%s)
    [ $((now - start)) -ge "$t" ] && { fail "timed out (${t}s) waiting for phase=$target (last: ${p:-<none>})"; return 1; }
    printf '\r   waiting: phase=%-22s (%ss)' "${p:-<none>}" "$((now - start))"
    sleep 5
  done
}

wait_leave_phase() { # wait until phase != $1 (repave kicks off)
  local from="$1" t="${2:-120}" start now
  start=$(date +%s)
  while [ "$(phase)" = "$from" ]; do
    now=$(date +%s)
    [ $((now - start)) -ge "$t" ] && { fail "phase never left $from within ${t}s — repave did not start"; return 1; }
    sleep 3
  done
}

os_pvcs() { # list this instance's OS disk PVC names (exact or -suffixed)
  kubectl get pvc -n "$NS" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null |
    grep -E "^pg-${ID}-os(-|$)" || true
}

resolve_dbname() {
  # Mirrors the controller's own fallback exactly (dbinstance_controller.go:
  # dbName := inst.Spec.DBName; if dbName == "" { dbName = id }) — so a YAML
  # with an explicit dbName (e.g. "orders") resolves correctly instead of
  # incorrectly assuming the database is named after the instance.
  local d
  d=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.dbName}' 2>/dev/null)
  echo "${d:-$ID}"
}

db_exec() { # db_exec <sql>  (uses current endpoint + secret)
  local ep pw user dbname
  ep=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.endpoint.address}')
  # Secret keys are admin_user / admin_password (typed_client.go
  # ensureCredentialsSecret) — NOT "password". PGUSER, if the caller set it
  # explicitly, wins; otherwise use the actual admin_user from the secret so
  # this works for any masterUsername, not just the "dbadmin" default.
  user=$(kubectl get secret "pg-${ID}-credentials" -n "$NS" -o jsonpath='{.data.admin_user}' | base64 -d)
  pw=$(kubectl get secret "pg-${ID}-credentials" -n "$NS" -o jsonpath='{.data.admin_password}' | base64 -d)
  dbname="${DBNAME:-$(resolve_dbname)}"
  [ -n "$ep" ] || { fail "no endpoint address on DBInstance"; return 1; }
  [ -n "$pw" ] || { fail "empty admin_password from secret pg-${ID}-credentials — secret missing/wrong keys?"; return 1; }
  # Server's pg_hba.conf is hostssl-only (cloudinit.go bootstrap.sh); force SSL
  # explicitly instead of relying on libpq's "prefer" negotiation.
  PGPASSWORD="$pw" PGSSLMODE=require psql -h "$ep" -U "${PGUSER:-$user}" -d "$dbname" -tAc "$1" 2>&1
}

apply_yaml() {
  # --validate=false: kubectl's client-side validation needs an OpenAPI
  # download from the apiserver, which fails on some clusters ("proto: cannot
  # parse invalid wire-format data" — kubectl/apiserver version skew, common
  # against Harvester). The apiserver still validates server-side.
  kubectl apply -f "$YAML" --validate=false
}

check_yaml_matches_id() {
  # Every assertion below keys off $ID (PVC names, annotate target, secret
  # name), so the manifest must create exactly that DBInstance or the script
  # polls a name that never appears and hangs until TIMEOUT.
  local yname yns
  yname=$(awk '/^metadata:/{m=1;next} m&&/^[^ ]/{m=0} m&&/^  name:/{print $2;exit}' "$YAML")
  yns=$(awk '/^metadata:/{m=1;next} m&&/^[^ ]/{m=0} m&&/^  namespace:/{print $2;exit}' "$YAML")
  [ "$yname" = "$ID" ] || die "YAML metadata.name='$yname' but ID='$ID' — set ID=$yname or edit the YAML"
  [ -z "$yns" ] || [ "$yns" = "$NS" ] || die "YAML namespace='$yns' but NS='$NS' — they must match"
}

provision_probe() { # provision_probe <instance-name> <engineVersion>
  # Provisions a disposable DBInstance cloned from $ID's own class/storage/
  # network (so E3 needs no extra YAML input), waits for it to settle into
  # Available or Failed, and prints "<phase>|<message>". Uses a local `id`
  # distinct from the global $ID/$STATE/$LOG on purpose — this never touches
  # the instance under test.
  local id="$1" engine="$2"
  local netref class storage
  netref=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.networkRef}')
  class=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.dbInstanceClass}')
  storage=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.allocatedStorage}')
  cat <<YAML | kubectl apply -f - --validate=false >/dev/null
apiVersion: dbaas.opencloud.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: ${id}
  namespace: ${NS}
spec:
  dbInstanceClass: ${class}
  allocatedStorage: ${storage}
  engineVersion: "${engine}"
  dbName: eoltest
  masterUsername: dbadmin
  manageMasterUserPassword: true
  networkRef: ${netref}
  running: true
YAML
  local t=0 p=""
  while [ $t -lt 180 ]; do
    p=$(kubectl get dbinstance "$id" -n "$NS" -o jsonpath='{.status.provisioningPhase}' 2>/dev/null)
    { [ "$p" = "Available" ] || [ "$p" = "Failed" ]; } && break
    sleep 5; t=$((t+5))
  done
  printf '%s|%s' "$p" "$(kubectl get dbinstance "$id" -n "$NS" -o jsonpath='{.status.message}' 2>/dev/null)"
}

# ---------- stages ----------
stage1() {
  say "T1: provision baseline"
  [ -n "${YAML:-}" ] || die "set YAML=<path to DBInstance manifest>"
  check_yaml_matches_id
  apply_yaml || die "apply failed"
  wait_phase "Available" || die "instance never became Available"
  echo

  local pvcs; pvcs=$(os_pvcs)
  [ -n "$pvcs" ] && pass "OS PVC exists: $pvcs" || fail "no OS PVC found"
  kubectl get pvc "pg-${ID}-data" -n "$NS" >/dev/null 2>&1 \
    && pass "data PVC pg-${ID}-data exists" || fail "data PVC missing"

  local base_pvc; base_pvc=$(os_pvcs | head -1)
  local base_sc; base_sc=$(kubectl get pvc "$base_pvc" -n "$NS" -o jsonpath='{.spec.storageClassName}' 2>/dev/null)
  {
    echo "baseline_os_pvc=$base_pvc"
    echo "baseline_os_sc=$base_sc"
  } > "$STATE"
  kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.appliedSpec.imageRevision}' \
    | xargs -I{} sh -c 'echo "baseline_rev={}" >> '"$STATE"

  if [ "${SKIP_DB:-0}" != "1" ]; then
    say "T2: seed data"
    local out; out=$(db_exec "CREATE TABLE IF NOT EXISTS repave_test(x int); INSERT INTO repave_test VALUES (1);")
    echo "$out" | grep -q "INSERT" && pass "seeded repave_test" || fail "seed failed: $out"
  else
    echo "(SKIP_DB=1: skipping seed)"
  fi

  say "stage1 done — now switch manager.yaml to the new OS stream and 'make deploy', then run stage2"
}

stage2() {
  [ -f "$STATE" ] || die "no state file $STATE — run stage1 first"
  # shellcheck disable=SC1090
  source "$STATE"

  say "T3: drift detection (OSUpdateAvailable)"
  local t=0 cond=""
  while [ $t -lt 300 ]; do
    cond=$(kubectl get dbinstance "$ID" -n "$NS" \
      -o jsonpath='{.status.conditions[?(@.type=="OSUpdateAvailable")].status}' 2>/dev/null)
    [ "$cond" = "True" ] && break; sleep 10; t=$((t+10))
  done
  [ "$cond" = "True" ] && pass "OSUpdateAvailable=True" || fail "OSUpdateAvailable never appeared (did you redeploy the controller?)"

  say "T4: repave"
  kubectl annotate dbinstance "$ID" -n "$NS" "$ANNOT" --overwrite || die "annotate failed"
  wait_leave_phase "Available" || die "repave never started"
  wait_phase "Available" || die "repave never completed"
  echo

  local pvcs count new_pvc
  pvcs=$(os_pvcs); count=$(echo "$pvcs" | grep -c . || true); new_pvc=$(echo "$pvcs" | head -1)
  [ "$count" = "1" ] && pass "exactly one OS PVC: $new_pvc" || fail "expected 1 OS PVC, got: $pvcs"
  [ "$new_pvc" != "$baseline_os_pvc" ] && pass "PVC name changed ($baseline_os_pvc → $new_pvc)" \
    || fail "PVC name unchanged — disk was NOT swapped"
  echo "$new_pvc" | grep -qE "^pg-${ID}-os-." && pass "new name is revision-suffixed" \
    || fail "new PVC not revision-suffixed: $new_pvc"

  local sc rev imgid; sc=$(kubectl get pvc "$new_pvc" -n "$NS" -o jsonpath='{.spec.storageClassName}')
  rev=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.appliedSpec.imageRevision}')
  imgid=$(kubectl get pvc "$new_pvc" -n "$NS" -o jsonpath='{.metadata.annotations.harvesterhci\.io/imageId}' 2>/dev/null)
  echo "   PVC storageClass: $sc | imageId: $imgid | appliedSpec.imageRevision: $rev"
  [ "$rev" != "$baseline_rev" ] && pass "imageRevision updated ($baseline_rev → $rev)" || fail "imageRevision unchanged"
  # storageClass NAMING is not a stable signal — Harvester auto-generates
  # names like "longhorn-image-rnrmm" for UI-uploaded images, unrelated to any
  # revision string (see resolveVMImage in typed_client.go). What actually
  # proves the disk changed lineage is that the storageClass differs from the
  # pre-repave baseline; the imageId annotation is the authoritative pointer
  # to which VirtualMachineImage backs it (cross-check in Harvester UI).
  [ -n "$sc" ] && [ "$sc" != "$baseline_os_sc" ] \
    && pass "PVC storageClass changed ($baseline_os_sc → $sc) — new disk lineage confirmed" \
    || fail "PVC storageClass unchanged ($sc) — disk was not actually reprovisioned from a new image"
  [ -n "$imgid" ] && pass "PVC carries imageId annotation: $imgid (verify this is the new image in Harvester UI)" \
    || fail "PVC missing harvesterhci.io/imageId annotation"

  [ -z "$(kubectl get dv -n "$NS" -o name 2>/dev/null | grep "pg-${ID}-os")" ] \
    && pass "no stray DataVolumes" || fail "stray DataVolume present"
  [ -z "$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.metadata.annotations.dbaas\.opencloud\.wso2\.com/repave-trigger}')" ] \
    && pass "repave-trigger annotation cleared" || fail "annotation still present"
  [ "$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="OSUpdateAvailable")].status}')" != "True" ] \
    && pass "OSUpdateAvailable condition removed" || fail "OSUpdateAvailable still True"

  if [ "${SKIP_DB:-0}" != "1" ]; then
    say "T5: data survived"
    local out; out=$(db_exec "SELECT x FROM repave_test;")
    echo "$out" | grep -q "^1$" && pass "repave_test row intact — pgdata preserved" \
      || fail "data check failed: $out"
  fi

  say "T6: second repave (idempotency — this used to hang the VM)"
  kubectl annotate dbinstance "$ID" -n "$NS" "$ANNOT" --overwrite
  wait_leave_phase "Available" 180 && wait_phase "Available" || die "second repave hung — REGRESSION"
  echo
  [ "$(os_pvcs | head -1)" = "$new_pvc" ] && pass "same disk after second repave ($new_pvc)" \
    || fail "disk changed on same-image repave: $(os_pvcs)"

  echo "post_repave_pvc=$new_pvc" >> "$STATE"
  say "stage2 done"
}

stage3() {
  say "T9: delete + leak scan"
  kubectl delete dbinstance "$ID" -n "$NS" --timeout=180s || die "delete failed (deletionProtection on?)"
  sleep 20
  local leaks
  leaks=$(kubectl get vm,vmi,dv,pvc,svc,endpoints,servicemonitor,secret -n "$NS" -o name 2>/dev/null | grep "pg-${ID}" || true)
  [ -z "$leaks" ] && pass "no leftovers — teardown clean" || fail "LEAKED RESOURCES:"$'\n'"$leaks"

  say "T10: re-apply same name (old-disk-reattach regression)"
  [ -n "${YAML:-}" ] || die "set YAML=<path to DBInstance manifest>"
  check_yaml_matches_id
  local before; before=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  apply_yaml || die "re-apply failed"
  wait_phase "Available" || die "re-applied instance never became Available"
  echo

  local created; created=$(kubectl get pvc "pg-${ID}-os" -n "$NS" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null)
  [ -n "$created" ] && [[ "$created" > "$before" || "$created" == "$before" ]] \
    && pass "fresh OS PVC created at $created" || fail "OS PVC missing or predates re-apply ($created) — old disk reattached?"

  if [ "${SKIP_DB:-0}" != "1" ]; then
    local out; out=$(db_exec "SELECT x FROM repave_test;")
    echo "$out" | grep -q "does not exist" && pass "repave_test absent — genuinely fresh database" \
      || fail "old data visible on re-applied instance: $out"
  fi
  say "stage3 done"
}

stage4() {
  say "E1: drift detection — PGVersionEOL (not OSUpdateAvailable)"
  local t=0 cond=""
  while [ $t -lt 300 ]; do
    cond=$(kubectl get dbinstance "$ID" -n "$NS" \
      -o jsonpath='{.status.conditions[?(@.type=="PGVersionEOL")].status}' 2>/dev/null)
    [ "$cond" = "True" ] && break; sleep 10; t=$((t+10))
  done
  [ "$cond" = "True" ] && pass "PGVersionEOL=True" \
    || fail "PGVersionEOL never appeared — did you publish the EOL image revision, point LatestBakedImages at it (Validated: true), and make deploy?"
  local osupd; osupd=$(kubectl get dbinstance "$ID" -n "$NS" \
    -o jsonpath='{.status.conditions[?(@.type=="OSUpdateAvailable")].status}' 2>/dev/null)
  [ "$osupd" != "True" ] && pass "OSUpdateAvailable correctly NOT set — engineVersion is EOL, not just behind" \
    || fail "OSUpdateAvailable=True but engineVersion is missing from the new image — should be PGVersionEOL instead"

  say "E2: repave is blocked at Step 0 — VM and disk left untouched"
  local pre_pvc vmname
  pre_pvc=$(os_pvcs | head -1)
  vmname=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.resources.vmName}')
  kubectl annotate dbinstance "$ID" -n "$NS" "$ANNOT" --overwrite || die "annotate failed"

  # failBlockedRepave (dbinstance_controller.go) clears repave-trigger on this
  # exact guard, so this message is now stable — before that fix, the very
  # next reconcile saw the annotation still "now" but ProvisioningPhase
  # already Failed, and the OTHER guard overwrote this message with a
  # generic "requires ProvisioningPhase=Available" within under a second.
  t=0; local msg=""
  while [ $t -lt 90 ]; do
    msg=$(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.status.message}' 2>/dev/null)
    echo "$msg" | grep -q "RepaveBlockedPGVersionEOL" && break
    sleep 5; t=$((t+5))
  done
  echo "$msg" | grep -q "RepaveBlockedPGVersionEOL" && pass "repave blocked before any destructive step: $msg" \
    || fail "expected RepaveBlockedPGVersionEOL in status.message within 90s, got: $msg"

  local post_pvc post_vmi_phase
  post_pvc=$(os_pvcs | head -1)
  [ "$post_pvc" = "$pre_pvc" ] && pass "OS PVC unchanged ($post_pvc) — no disk swap was attempted" \
    || fail "OS PVC changed ($pre_pvc → $post_pvc) — a blocked repave must not touch storage"
  post_vmi_phase=$(kubectl get vmi "$vmname" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$post_vmi_phase" = "Running" ] && pass "VMI still Running ($vmname) — VM was never stopped" \
    || fail "VMI phase = '${post_vmi_phase:-<none>}', expected Running — a blocked repave must not touch the VM"

  # failBlockedRepave already cleared the annotation above, so this is now a
  # harmless no-op kept as a defensive fallback (>/dev/null covers the "already
  # absent" case). Confirm the instance settles on its own — proof that
  # nothing was actually broken, just correctly refused.
  kubectl annotate dbinstance "$ID" -n "$NS" "${ANNOT%=*}-" >/dev/null 2>&1
  # Manual poll rather than wait_phase: wait_phase's own timeout path already
  # calls fail() and this must not die() on failure — E3 below is independent
  # and worth running regardless of whether recovery completed in time.
  t=0; local recovered=""
  while [ $t -lt 120 ]; do
    recovered=$(phase)
    [ "$recovered" = "Available" ] && break
    sleep 5; t=$((t+5))
  done
  [ "$recovered" = "Available" ] && pass "instance recovered to Available after clearing the annotation" \
    || fail "instance did not recover to Available after clearing repave-trigger (phase=${recovered:-<none>})"

  say "E3: new-instance rules under the EOL stream"
  local id_ok="${ID}-eol-pg18" result_ok phase_ok
  result_ok=$(provision_probe "$id_ok" "18")
  phase_ok="${result_ok%%|*}"
  [ "$phase_ok" = "Available" ] && pass "new instance on engineVersion=18 (current stream's version) provisions fine" \
    || fail "engineVersion=18 instance did not reach Available (phase=$phase_ok): ${result_ok#*|}"
  kubectl delete dbinstance "$id_ok" -n "$NS" --timeout=120s >/dev/null 2>&1

  local id_bad="${ID}-eol-pg17" result_bad phase_bad msg_bad
  result_bad=$(provision_probe "$id_bad" "17")
  phase_bad="${result_bad%%|*}"; msg_bad="${result_bad#*|}"
  { [ "$phase_bad" = "Failed" ] && echo "$msg_bad" | grep -q "UnsupportedEngineVersion"; } \
    && pass "new instance on engineVersion=17 (EOL'd out of the stream) correctly rejected: $msg_bad" \
    || fail "expected Failed/UnsupportedEngineVersion for engineVersion=17, got phase=$phase_bad msg=$msg_bad"
  kubectl delete dbinstance "$id_bad" -n "$NS" --timeout=120s >/dev/null 2>&1

  say "E4: cross-instance data migration (manual — P008 playbook)"
  echo "Not automated: this moves data between two independent instances and"
  echo "needs a human to eyeball row counts, not a scripted assertion. Create a"
  echo "migration-target instance on the new PG version, then run:"
  echo
  echo "  OLD_IP=\$(kubectl get dbinstance $ID -n $NS -o jsonpath='{.status.endpoint.address}')"
  echo "  NEW_IP=\$(kubectl get dbinstance <new-instance> -n $NS -o jsonpath='{.status.endpoint.address}')"
  echo "  pg_dumpall -h \$OLD_IP -U $(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.masterUsername}') --globals-only > globals.sql"
  echo "  pg_dump -h \$OLD_IP -U $(kubectl get dbinstance "$ID" -n "$NS" -o jsonpath='{.spec.masterUsername}') -Fc $(resolve_dbname) > database.dump"
  echo "  # then psql -f globals.sql and pg_restore against \$NEW_IP — see P008 §2 for the full template"

  say "stage4 done"
}

case "$STAGE_ARG" in
  stage1) stage1 ;;
  stage2) stage2 ;;
  stage3) stage3 ;;
  stage4) stage4 ;;
  all)
    stage1
    printf '\n\033[1;33m>> Now edit manager.yaml (new OS stream) and run: make deploy IMG=<img>\n>> Press Enter when the controller rollout is complete...\033[0m'
    read -r
    stage2
    stage3
    ;;
esac

if [ "$FAILED" = "0" ]; then
  record_summary "PASS"
  printf '\n\033[1;32mALL CHECKS PASSED\033[0m (results: %s)\n' "$LOG"
  exit 0
else
  record_summary "FAIL"
  printf '\n\033[1;31mSOME CHECKS FAILED\033[0m (results: %s)\n' "$LOG"
  exit 1
fi
