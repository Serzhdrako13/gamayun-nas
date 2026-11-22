// lib/FoundPools.svelte: пулы на дисках после переустановки.
export default {
  title: 'Found pools',
  disks: 'disks: {n}',
  integrity: ', integrity protection',
  known: 'A pool with this name already exists — cannot import.',
  busy: 'The pool volume is already mounted.',
  confirm: 'Type the pool name “{name}” to import',
  degraded: 'Import an incomplete pool (not all disks found)',
  import: 'Import',
  importing: 'Importing…',
  done: 'Pool {name} imported',
  failed: 'Import failed',
};
