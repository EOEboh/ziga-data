#!/usr/bin/env bash
#
# backup-ziga.sh — nightly WAL/journal-safe backup of the ziga SQLite database
# to the existing Cloudflare R2 bucket via rclone.
#
# WHAT:  Takes a consistent snapshot with `sqlite3 .backup` (never copies the
#        live file), gzips it with a UTC timestamp, uploads to R2, and prunes
#        uploads older than 30 days.
# WHERE: Install to /opt/ziga/backup-ziga.sh (chmod 750, owned by ziga). Driven
#        nightly by /etc/cron.d/ziga-backup (see deploy/backup-ziga.cron).
#        See deploy/RUNBOOK.md §e for install + the mandatory restore test.
#
# CONFIG (from the environment — the cron file / rclone.conf supply these; no
# secrets live in this script):
#   RCLONE_REMOTE   rclone remote name for R2 (e.g. "r2"). Required.
#   R2_BUCKET       destination bucket. Required.
#   R2_PREFIX       key prefix within the bucket (e.g. "ziga/backups"). Required.
#   RCLONE_CONFIG   optional path to rclone.conf (when not the ziga user default).
#   BACKUP_DRY_RUN  when "1", skip the upload+prune and just print the actions
#                   (used by CI and for local testing).

set -euo pipefail

DB_PATH="${DB_PATH:-/opt/ziga/ziga.db}"
DRY_RUN="${BACKUP_DRY_RUN:-0}"

log() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

# Validate required config up front — before creating any temp files — so a
# misconfigured cron fails loudly and nonzero rather than uploading nowhere.
# In dry-run we still require them so we exercise the exact same code path.
: "${RCLONE_REMOTE:?RCLONE_REMOTE is required}"
: "${R2_BUCKET:?R2_BUCKET is required}"
: "${R2_PREFIX:?R2_PREFIX is required}"

DEST="${RCLONE_REMOTE}:${R2_BUCKET}/${R2_PREFIX}"

# Timestamped names. The mktemp template keeps the X's at the very end so both
# GNU (server) and BSD (dev) mktemp accept it; the extension is irrelevant to
# sqlite3 / gzip.
STAMP="$(date -u +%Y%m%d-%H%M%S)"
SNAPSHOT="$(mktemp -t ziga-backup.XXXXXX)"
GZ="${SNAPSHOT}.gz"
OBJECT="ziga-${STAMP}.db.gz"

# Clean up the local temp files on any exit, but PRESERVE the exit status: a
# bare `rm` as the trap's last command would reset $? to 0 and hide a snapshot
# or upload failure from cron. Capturing rc and re-exiting keeps failures loud.
cleanup() {
    rc=$?
    rm -f "${SNAPSHOT}" "${GZ}"
    exit "${rc}"
}
trap cleanup EXIT

# rclone honours RCLONE_CONFIG from the environment automatically; we only note
# it here for the dry-run log.
if [[ -n "${RCLONE_CONFIG:-}" ]]; then
    log "using rclone config: ${RCLONE_CONFIG}"
fi

# 1. Consistent snapshot. The .backup command uses SQLite's online backup API,
#    which is safe against concurrent writers and correct for both WAL and
#    rollback-journal modes — unlike a raw cp of the live file.
log "snapshotting ${DB_PATH} -> ${SNAPSHOT}"
sqlite3 "${DB_PATH}" ".backup '${SNAPSHOT}'"

# 2. Compress.
log "compressing -> ${GZ}"
gzip -f "${SNAPSHOT}"

# 3. Upload + prune (or describe them in dry-run).
if [[ "${DRY_RUN}" == "1" ]]; then
    log "DRY RUN: would upload  ${GZ}  ->  ${DEST}/${OBJECT}"
    log "DRY RUN: would prune   ${DEST}/  objects older than 30d"
    log "dry run complete, no changes made to R2"
    exit 0
fi

log "uploading -> ${DEST}/${OBJECT}"
rclone copyto "${GZ}" "${DEST}/${OBJECT}"

log "pruning ${DEST}/ objects older than 30d"
rclone delete --min-age 30d "${DEST}/"

log "backup complete: ${OBJECT}"
