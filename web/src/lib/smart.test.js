import { test } from 'node:test';
import assert from 'node:assert/strict';
import { smartBadge, smartCounts, series } from './smart.js';

test('smartBadge: уровни, температура, без SMART', () => {
  assert.deepEqual(smartBadge(undefined), ['не опрошен', '']);
  assert.deepEqual(smartBadge({ level: 'unknown', supported: false }), ['нет SMART', '']);
  assert.deepEqual(smartBadge({ level: 'ok', supported: true, temperature_c: 38 }), ['в норме · 38 °C', 'ok']);
  assert.deepEqual(smartBadge({ level: 'warning', supported: true }), ['внимание', 'warn']);
  assert.deepEqual(smartBadge({ level: 'critical', supported: true, temperature_c: 70 }), ['неисправность · 70 °C', 'bad']);
  assert.deepEqual(smartBadge({ level: 'unknown', supported: true, standby: true }), ['спит', '']);
  assert.deepEqual(smartBadge({ level: 'unknown', supported: false, disabled: true }), ['SMART выключен', 'warn']);
  assert.deepEqual(smartBadge({ level: 'unknown', supported: false, disabled: true, manual_off: true }), ['выключен вручную', '']);
});

test('smartCounts: выключенный SMART', () => {
  const c = smartCounts([
    { name: 'Диск 1', smart: { level: 'unknown', disabled: true } },
    { name: 'Диск 2', smart: { level: 'unknown', disabled: true, manual_off: true } },
  ]);
  assert.equal(c.warning, 1);
  assert.equal(c.unknown, 1);
  assert.equal(c.problems[0].name, 'Диск 1');
});

test('smartCounts: счёт, максимум температуры, проблемы — critical первыми', () => {
  const c = smartCounts([
    { name: 'Диск 1', smart: { level: 'ok', supported: true, temperature_c: 35 } },
    { name: 'Диск 2', smart: { level: 'warning', supported: true, temperature_c: 57, reasons: ['температура 57 °C'] } },
    { name: 'Диск 3', smart: { level: 'critical', supported: true, reasons: ['самооценка: неисправен'] } },
    { name: 'Диск 4', smart: { level: 'unknown', supported: false } },
    { name: 'Диск 5' },
  ]);
  assert.equal(c.ok, 1);
  assert.equal(c.warning, 1);
  assert.equal(c.critical, 1);
  assert.equal(c.unknown, 2);
  assert.equal(c.maxTemp, 57);
  assert.deepEqual(c.problems.map((p) => p.name), ['Диск 3', 'Диск 2']);
  assert.equal(smartCounts(null).maxTemp, null);
});

test('series: масштаб, разрыв по пропуску и по времени', () => {
  const t0 = Date.UTC(2026, 10, 1);
  const p = (m, v) => ({ t: new Date(t0 + m * 60000).toISOString(), temperature_c: v });
  const pts = [p(0, 30), p(30, 40), p(60, null), p(90, 50), p(600, 30)];
  const s = series(pts, 'temperature_c', { from: t0, to: t0 + 600 * 60000, w: 600, h: 100, lo: 30, hi: 40, gap: 120 * 60000, pad: 0 });
  assert.equal(s.hi, 50); // расширено по данным
  assert.equal(s.lines.length, 3); // пропуск и разрыв > 2 ч
  assert.equal(s.lines[0], '0.0,100.0 30.0,50.0');
  assert.equal(s.lines[1], '90.0,0.0');
  assert.equal(s.lines[2], '600.0,100.0');
  assert.deepEqual(series([], 'temperature_c', { from: 0, to: 1, w: 10, h: 10, lo: 0, hi: 1 }).lines, []);
});
