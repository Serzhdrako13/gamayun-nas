// раздел справки для блока «Что происходит» карточки пула —
// по полям статуса, а не по тексту status.issues (коды issues — этап 2).
// Возвращает id из PLANNED (parse.js); ненаписанный раздел HelpButton скрывает.
export function poolTopic(pool) {
  const st = pool?.status;
  const h = st?.health;
  const p = pool?.protection || {};
  if (h === 'missing') return 'storage.pools.missing';
  if (h === 'failed') return 'storage.pools.failed';
  if (h === 'unknown') return 'storage.pools.unknown';
  if (h === 'degraded') return 'storage.pools.incomplete';
 // аварийный RO Btrfs (health warning) — тот же раздел, глава об аварийном переходе.
  if (h === 'warning' || st?.emergency_read_only) return 'storage.pools.readonly';
  if ((pool?.pending || pool?.grow_pending) && !pool?.busy) return 'storage.pools.pending';
  if (p.write_blocked || p.read_only || p.override) return 'storage.pools.readonly';
  if ((st?.integrity_errors || 0) > 0) return 'storage.pools.integrity';
  return null;
}
