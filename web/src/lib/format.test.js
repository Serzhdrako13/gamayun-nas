import { test } from 'node:test';
import assert from 'node:assert/strict';
import { bytes, freeDisks, protectionBanner, failures, health, syncText, syncEta } from './format.js';

test('bytes', () => {
  assert.equal(bytes(0), '0 Б');
  assert.equal(bytes(1024 ** 3), '1,00 ГиБ');
  assert.equal(bytes(4 * 1024 ** 4), '4,00 ТиБ');
  assert.equal(bytes(null), '—');
});

test('failures', () => {
  assert.match(failures(0), /ни одного/);
  assert.match(failures(1), /1 отказ диска/);
  assert.match(failures(2), /2 отказа дисков/);
});

test('freeDisks: только полный снимок, id и роль unknown', () => {
  const d = [
    { id: 'a', system_role: 'unknown', size_bytes: 5 },
    { id: 'b', system_role: 'root', size_bytes: 5 },
    { system_role: 'unknown', size_bytes: 5 },
  ];
  assert.deepEqual(freeDisks({ complete: true, disks: d }).map((x) => x.id), ['a']);
  assert.deepEqual(freeDisks({ complete: false, disks: d }), []);
});

test('protectionBanner', () => {
  assert.equal(protectionBanner({ protection: { read_only: false } }), null);
  assert.equal(protectionBanner({ protection: { read_only: true, reason: 'x' } }).kind, 'bad');
  const o = protectionBanner({ protection: { read_only: false, override: { expires_at: '2026-11-06T00:00:00Z' } } });
  assert.equal(o.override, true);
  // Защита выключена — баннер всегда; при исчерпании — bad и текст о риске.
  const off = protectionBanner({ protection: { enabled: false, read_only: false } });
  assert.equal(off.kind, 'warn');
  assert.equal(off.off, true);
  assert.match(off.text, /Защита пула выключена/);
  const offEx = protectionBanner({ protection: { enabled: false, redundancy_exhausted: true } });
  assert.equal(offEx.kind, 'bad');
  assert.match(offEx.text, /потере данных/);
  const offEm = protectionBanner({ protection: { enabled: false }, status: { emergency_read_only: true } });
  assert.match(offEm.text, /аварийно.*Защита пула выключена/);
  assert.equal(protectionBanner({ protection: { enabled: true } }), null);
});

test('health', () => {
  assert.equal(health({ status: { health: 'degraded' } })[1], 'warn');
  assert.equal(health({ status: null })[1], 'unknown');
});

test('health: тексты по словарю', () => {
  assert.equal(health({ status: { health: 'missing' } })[0], 'Диски не найдены');
  assert.equal(health({ status: { health: 'degraded' } })[0], 'Неполный (диск выбыл)');
  assert.equal(health({ status: { health: 'reshaping' } })[0], 'Перестройка');
  assert.equal(health({ status: { health: 'warning' } })[0], 'Только чтение (ошибка записи)');
  assert.equal(health({ status: { health: 'warning' } })[1], 'warn');
  const b = protectionBanner({ status: { health: 'warning', emergency_read_only: true }, protection: { read_only: true } });
  assert.equal(b.kind, 'bad');
  assert.match(b.text, /аварийно/);
});

test('ход начальной синхронизации', () => {
  assert.equal(syncText({ status: { health: 'ok' } }), null);
  const p = (sync) => ({ status: { initial_sync: true, sync } });
  assert.equal(syncText(p({ stage: 'integrity', percent: 12.7 })), '12 % (расчёт контрольных сумм, оценка времени появится через минуту)');
  assert.equal(syncText(p({ stage: 'mirror', percent: 80, eta_min: 135.2 })), '80 % (синхронизация дисков, осталось ~2 ч 15 мин)');
  assert.equal(syncEta(0.2), '~1 мин');
});
