#!/usr/bin/env bash
#
# backup-ziga.sh — nightly journal-safe backup of the ziga SQLite database to
# the Cloudflare R2 bucket via rclone.
#
# WHAT:  Takes a consistent snapshot with `sqlite3 .backup` (never copies the
#        live file), gzips it with a UTC timestamp, uploads it to R2, and prunes
#        uploads older than the retention window.
# WHERE: Install to /opt/ziga/backup-ziga.sh (chmod 750, owned by deploy). Driven
#        nightly by ziga-backup.timer (see deploy/ziga-backup.timer).
#        See deploy/RUNBOOK.md §e for install + the mandatory restore test.
#
# WHY THE deploy USER: the app runs as `ziga`, but only `deploy` can read the
# rclone config (and therefore the R2 credentials), so the backup runs as deploy.
# That means deploy needs read access to the database — RUNBOOK.md §e step 1.
#
# CONFIG (all of it is in the variables below; every one can be overridden from
# the environment for testing. No secrets live in this script — the rclone config
# is referenced by path only):
#   DB_PATH         the SQLite database to back up.
#   RCLONE_CONF     path to rclone.conf, passed explicitly as `rclone --config`.
#   R2_DEST         rclone destination, "<remote>:<bucket>" (no trailing slash).
#   RETENTION_DAYS  objects in R2_DEST older than this many days are pruned.
#   BACKUP_DRY_RUN  when "1", skip the upload+prune and just print the actions
#                   (used for local testing and for a first pass on the server).

set -euo pipefail

DB_PATH="${DB_PATH:-/opt/ziga/ziga.db}"
RCLONE_CONF="${RCLONE_CONF:-/home/deploy/.config/rclone/rclone.conf}"
R2_DEST="${R2_DEST:-r2:ziga-backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
DRY_RUN="${BACKUP_DRY_RUN:-0}"

# hookdrop's R2 flags. They are split because they are NOT interchangeable:
# --no-check-dest is a copy/sync-only flag and `rclone delete` rejects it as an
# unknown flag, which would fail the prune every night.
#
# --s3-no-check-bucket also means rclone will NOT create the bucket if it is
# missing: R2_DEST's bucket must already exist or every run fails with
# NoSuchBucket. It is created once by hand — see RUNBOOK.md §e step 3.
R2_FLAGS=(--config "${RCLONE_CONF}" --s3-no-check-bucket)
COPY_FLAGS=("${R2_FLAGS[@]}" --no-check-dest)

log() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

die() {
    log "ERROR: $*" >&2
    exit 1
}

# Validate config up front — before creating any temp files — so a broken setup
# fails loudly and nonzero rather than backing up nothing in silence. Dry-run
# checks these too, so it exercises the exact same failure paths.
[[ -r "${RCLONE_CONF}" ]] || die "rclone config not readable: ${RCLONE_CONF}"
[[ -r "${DB_PATH}" ]] || die "database not readable: ${DB_PATH} (is deploy in the ziga group? see RUNBOOK §e)"

# Timestamped names. The mktemp template keeps the X's at the very end so both
# GNU (server) and BSD (dev) mktemp accept it; the extension is irrelevant to
# sqlite3 / gzip.
STAMP="$(date -u +%Y%m%d-%H%M%S)"
SNAPSHOT="$(mktemp -t ziga-backup.XXXXXX)"
GZ="${SNAPSHOT}.gz"
OBJECT="ziga-${STAMP}.db.gz"

# Clean up the local temp files on any exit, but PRESERVE the exit status: a
# bare `rm` as the trap's last command would reset $? to 0 and hide a snapshot
# or upload failure from the scheduler. Capturing rc and re-exiting keeps
# failures loud, so `systemctl status ziga-backup` reports Result: exit-code.
cleanup() {
    rc=$?
    rm -f "${SNAPSHOT}" "${GZ}"
    exit "${rc}"
}
trap cleanup EXIT

log "config: db=${DB_PATH} dest=${R2_DEST} retention=${RETENTION_DAYS}d rclone-config=${RCLONE_CONF}"

# 1. Consistent snapshot. The .backup command uses SQLite's online backup API,
#    which is safe against concurrent writers and correct for both WAL and
#    rollback-journal modes — unlike a raw cp of the live file.
log "snapshotting ${DB_PATH} -> ${SNAPSHOT}"
sqlite3 "${DB_PATH}" ".backup '${SNAPSHOT}'"

# 2. Compress.
log "compressing -> ${GZ}"
gzip -f "${SNAPSHOT}"

# 3. Upload + prune (or describe them in dry-run). The snapshot and gzip above
#    run in dry-run too, so a dry run proves the whole local pipeline works.
if [[ "${DRY_RUN}" == "1" ]]; then
    log "DRY RUN: would upload  ${GZ}  ->  ${R2_DEST}/${OBJECT}"
    log "DRY RUN:   rclone copyto ${COPY_FLAGS[*]}"
    log "DRY RUN: would prune   ${R2_DEST}/  objects older than ${RETENTION_DAYS}d"
    log "DRY RUN:   rclone delete ${R2_FLAGS[*]} --min-age ${RETENTION_DAYS}d"
    log "dry run complete, no changes made to R2"
    exit 0
fi

log "uploading -> ${R2_DEST}/${OBJECT}"
rclone copyto "${COPY_FLAGS[@]}" "${GZ}" "${R2_DEST}/${OBJECT}"

log "pruning ${R2_DEST}/ objects older than ${RETENTION_DAYS}d"
rclone delete "${R2_FLAGS[@]}" --min-age "${RETENTION_DAYS}d" "${R2_DEST}/"

log "backup complete: ${OBJECT}"
