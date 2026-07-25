#!/usr/bin/env bash
#
# backup.sh — nightly journal-safe backup of the ziga SQLite database to
# Cloudflare R2 via rclone.
#
# WHAT:  Takes a consistent snapshot with `sqlite3 .backup` (never copies the
#        live file), gzips it with a UTC timestamp, uploads it to R2, and prunes
#        uploads older than the retention window.
# WHERE: Install to /opt/ziga/backup.sh (chmod 750, owned by deploy) — the same
#        filename and position under /opt/<app>/ as hookdrop's backup.sh, so the
#        two are debugged the same way. Driven nightly by ziga-backup.timer.
#        See deploy/RUNBOOK.md §e for install + the mandatory restore test.
#
# WHY THE deploy USER: the app runs as `ziga`, but only `deploy` can read the
# rclone config (and therefore the R2 credentials), so the backup runs as deploy.
# That means deploy needs read access to the database — RUNBOOK.md §e step 1.
#
# RELATIONSHIP TO hookdrop's /opt/hookdrop/backup.sh: this deliberately mirrors
# its layout, object naming, rclone flags, and log file. It deliberately does NOT
# mirror four things, because hookdrop's version is weaker on each:
#   * `set -euo pipefail` here vs `set -e` there;
#   * an EXIT trap that preserves the real exit status (see cleanup below);
#   * a BACKUP_DRY_RUN mode;
#   * an explicit `rclone --config`, so the job does not depend on $HOME.
# Two further differences are ziga-specific rather than better or worse: ziga is
# not containerised, so it snapshots with a direct `sqlite3` instead of
# `docker exec`, and it uses its own bucket and its own scoped R2 token
# (RUNBOOK §e step 2).
#
# CONFIG (all of it is in the variables below; every one can be overridden from
# the environment for testing. No secrets live in this script — the rclone config
# is referenced by path only):
#   DB_PATH         the SQLite database to back up.
#   RCLONE_CONF     path to rclone.conf, passed explicitly as `rclone --config`.
#   R2_DEST         rclone destination, "<remote>:<bucket>" (no trailing slash).
#   RETENTION_DAYS  objects in R2_DEST older than this many days are pruned.
#   LOG_FILE        appended to, in addition to stdout/journald.
#   BACKUP_DRY_RUN  when "1", skip the upload+prune and just print the actions.

set -euo pipefail

DB_PATH="${DB_PATH:-/opt/ziga/ziga.db}"
RCLONE_CONF="${RCLONE_CONF:-/home/deploy/.config/rclone/rclone.conf}"
R2_DEST="${R2_DEST:-r2ziga:ziga-backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
LOG_FILE="${LOG_FILE:-/var/log/ziga-backup.log}"
DRY_RUN="${BACKUP_DRY_RUN:-0}"

# Underscore timestamp style, matching hookdrop's hookdrop_YYYYMMDD_HHMMSS.db.
DATE="$(date -u +%Y%m%d_%H%M%S)"

# A predictable /tmp path rather than mktemp — this is what lets the upload use
# `rclone copy`, which derives the object name from the local filename, exactly
# as hookdrop does. Predictable names in a shared /tmp would be a symlink-attack
# surface, so ziga-backup.service sets PrivateTmp=true and this script gets its
# own empty /tmp. Do not remove one without the other.
LOCAL_BACKUP="/tmp/ziga_${DATE}.db"
GZ="${LOCAL_BACKUP}.gz"
OBJECT="$(basename "${GZ}")"

# hookdrop's R2 flags. --no-check-dest is copy/sync-only: `rclone delete` rejects
# it as an unknown flag, so it goes on the upload only. --s3-no-check-bucket is
# redundant against a remote that already sets no_check_bucket = true (both r2
# and r2ziga do), but is passed anyway so the two apps' commands read alike.
COPY_FLAGS=(--config "${RCLONE_CONF}" --no-check-dest --s3-no-check-bucket --log-level INFO)
PRUNE_FLAGS=(--config "${RCLONE_CONF}" --s3-no-check-bucket --log-level INFO)

# Everything on stdout also lands in LOG_FILE, so `tail -f /var/log/ziga-backup.log`
# works the way it does for hookdrop while journald still captures the run. A
# logging problem must not cost us a working backup, so an unwritable file
# degrades to stdout-only instead of aborting.
if : >>"${LOG_FILE}" 2>/dev/null; then
    exec > >(tee -a "${LOG_FILE}") 2>&1
else
    printf '%s WARNING: %s is not writable, logging to stdout only\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${LOG_FILE}" >&2
fi

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

# Clean up the local temp files on any exit, but PRESERVE the exit status: a
# bare `rm` as the trap's last command would reset $? to 0 and hide a snapshot
# or upload failure from the scheduler. Capturing rc and re-exiting keeps
# failures loud, so `systemctl status ziga-backup` reports Result: exit-code.
cleanup() {
    rc=$?
    rm -f "${LOCAL_BACKUP}" "${GZ}"
    exit "${rc}"
}
trap cleanup EXIT

log "starting backup: ${OBJECT}"
log "config: db=${DB_PATH} dest=${R2_DEST} retention=${RETENTION_DAYS}d rclone-config=${RCLONE_CONF}"

# 1. Consistent snapshot. The .backup command uses SQLite's online backup API,
#    which is safe against concurrent writers and correct for both WAL and
#    rollback-journal modes — unlike a raw cp of the live file. hookdrop runs the
#    same command through `docker exec`; ziga is not containerised, so it is
#    called directly against the file.
log "snapshotting ${DB_PATH} -> ${LOCAL_BACKUP}"
sqlite3 "${DB_PATH}" ".backup '${LOCAL_BACKUP}'"

# 2. Compress. hookdrop uploads raw .db; ziga gzips because its database is much
#    smaller and SQLite compresses well. The restore test gunzips accordingly.
log "compressing -> ${GZ}"
gzip -f "${LOCAL_BACKUP}"

# 3. Upload + prune (or describe them in dry-run). The snapshot and gzip above
#    run in dry-run too, so a dry run proves the whole local pipeline works.
if [[ "${DRY_RUN}" == "1" ]]; then
    log "DRY RUN: would upload  ${GZ}  ->  ${R2_DEST}/${OBJECT}"
    log "DRY RUN:   rclone copy ${COPY_FLAGS[*]}"
    log "DRY RUN: would prune   ${R2_DEST}/  objects older than ${RETENTION_DAYS}d"
    log "DRY RUN:   rclone delete ${PRUNE_FLAGS[*]} --min-age ${RETENTION_DAYS}d"
    log "dry run complete, no changes made to R2"
    exit 0
fi

log "uploading -> ${R2_DEST}/${OBJECT}"
rclone copy "${GZ}" "${R2_DEST}/" "${COPY_FLAGS[@]}"

log "pruning ${R2_DEST}/ objects older than ${RETENTION_DAYS}d"
rclone delete "${R2_DEST}/" "${PRUNE_FLAGS[@]}" --min-age "${RETENTION_DAYS}d"

log "backup complete: ${OBJECT}"
