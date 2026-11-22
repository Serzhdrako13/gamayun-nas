import { test } from 'node:test';
import assert from 'node:assert/strict';
import { snapTime, crumbs, parentPath, joinPath, policyError, policySummary, restoreText } from './snapshots.js';

test('время снапшота в местном времени', () => {
  const d = new Date(2026, 10, 18, 9, 5);
  assert.equal(snapTime(d.toISOString()), '18.11.2026 09:05');
  assert.equal(snapTime('мусор'), '—');
});

test('пути просмотра снапшота', () => {
  assert.deepEqual(crumbs(''), [{ name: 'корень', path: '' }]);
  assert.deepEqual(crumbs('a/Отчёты').map((c) => c.path), ['', 'a', 'a/Отчёты']);
  assert.equal(parentPath('a/b/c'), 'a/b');
  assert.equal(parentPath('a'), '');
  assert.equal(joinPath('', 'x'), 'x');
  assert.equal(joinPath('a', 'x'), 'a/x');
});

test('проверка расписания', () => {
  assert.equal(policyError({ enabled: true, hourly: 24, daily: 7, weekly: 4, monthly: 6 }), '');
  assert.match(policyError({ enabled: true, hourly: 0, daily: 0, weekly: 0, monthly: 0 }), /ни один/);
  assert.equal(policyError({ enabled: false, hourly: 0, daily: 0, weekly: 0, monthly: 0 }), '');
  assert.match(policyError({ enabled: true, hourly: -1, daily: 0, weekly: 0, monthly: 0 }), /Ежечасные/);
  assert.match(policyError({ enabled: true, hourly: 1.5, daily: 0, weekly: 0, monthly: 0 }), /целое/);
  assert.match(policyError({ enabled: true, hourly: 1, daily: 999, weekly: 0, monthly: 0 }), /Ежедневные/);
});

test('подписи', () => {
  assert.equal(policySummary({ enabled: true, hourly: 24, daily: 7, weekly: 0, monthly: 6 }), 'хранить: 24 ч · 7 дн · 6 мес');
  assert.equal(policySummary({ enabled: false }), 'по расписанию — выключено');
  assert.equal(restoreText({ path: 'a (снапшот).txt', to_root: true, skipped: 2 }), 'Восстановлено как «a (снапшот).txt» (исходной папки нет — в корне); пропущено специальных файлов: 2');
});
