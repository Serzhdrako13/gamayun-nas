// Манифесты разделов корректны — новый раздел добавляется только сюда.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { SECTIONS } from '../shell/sections.js';
import { eventText, eventKind, notifyText } from '../shell/events.js';

const icons = readFileSync(new URL('./Icon.svelte', import.meta.url), 'utf8');
const views = readFileSync(new URL('../shell/views.js', import.meta.url), 'utf8');

test('манифесты разделов', () => {
  const ids = new Set();
  for (const s of SECTIONS) {
    assert.match(s.id, /^[a-z][a-z0-9-]*$/, s.id);
    assert.ok(!ids.has(s.id), 'повтор id ' + s.id);
    ids.add(s.id);
    assert.ok(s.title && s.desc, s.id);
    assert.ok(new RegExp(`\\b${s.icon}:`).test(icons), `значок ${s.icon} нет в Icon.svelte`);
    if (!s.soon) {
      assert.ok(s.nav.length > 0, s.id + ': пустое меню');
      for (const n of s.nav) assert.ok(new RegExp(`\\b${n.view}:`).test(views), `${s.id}: нет вида ${n.view} в views.js`);
    }
  }
});

test('подписи уведомлений', () => {
  assert.equal(notifyText({ type: 'array.degraded', subject: 'pool:test1' }), 'Массив RAID неполный: выбыл диск · пул test1');
  assert.equal(notifyText({ type: 'x.y' }), 'x.y');
  // Аварийный RO пула
  assert.equal(notifyText({ type: 'pool.emergency_ro', subject: 'pool:test1' }), 'Пул аварийно перешёл в режим только для чтения · пул test1');
  assert.equal(notifyText({ type: 'pool.protection_bypassed', subject: 'pool:test1' }), 'Запас избыточности исчерпан, защита пула выключена — пул остаётся на запись · пул test1');
  assert.equal(eventText({ type: 'pool.emergency_ro_cleared', data: { pool: 'test1' } }), 'Пул снова доступен для записи');
});

test('подписи событий', () => {
  assert.equal(eventText({ type: 'auth.login', data: { user: 'admin', remote: '10.0.0.5:5555' } }), 'Вход admin с 10.0.0.5');
  assert.equal(eventText({ type: 'config.backup.saved', data: { pool: 'data', file: 'x' } }), 'Копия настроек сохранена: data');
  assert.equal(eventText({ type: 'auth.unlock', data: { user: 'ann' } }), 'Блокировка входа снята ann');
  assert.equal(eventKind({ type: 'auth.login', data: { result: 'failure' } }), 'warn');
  assert.equal(eventText({ type: 'x.y' }), 'x.y');
  assert.equal(eventText({ type: 'disk.failed' }), 'Диск отказал');
  assert.equal(eventText({ type: 'system.reboot', data: '{"user":"admin"}' }), 'Перезагрузка сервера (admin)');
});

import { parseRoute, routeOf } from '../shell/route.js';
test('адрес оболочки', () => {
  assert.deepEqual(parseRoute(''), { section: 'home', nav: 0 });
  assert.deepEqual(parseRoute('#/pools?open=test1'), { section: 'storage', nav: 0 });
  assert.deepEqual(parseRoute('#/disks'), { section: 'storage', nav: 1 });
  assert.deepEqual(parseRoute('#/storage'), { section: 'storage', nav: 0 });
  assert.deepEqual(parseRoute('#/backup'), { section: 'home', nav: 0 }, 'раздел «скоро» не открывается');
  assert.deepEqual(parseRoute('#/files'), { section: 'files', nav: 0 }, 'раздел «Файлы»');
  assert.deepEqual(parseRoute('#/nope'), { section: 'home', nav: 0 });
  assert.equal(routeOf('storage', 1), '#/disks');
  assert.equal(routeOf('home'), '#/');
  for (const s of SECTIONS.filter((x) => !x.soon)) s.nav.forEach((_, i) => assert.deepEqual(parseRoute(routeOf(s.id, i)), { section: s.id, nav: i }));
});

import { mbps, rates, push, line, HISTORY } from '../shell/net.js';
import { eventParam } from '../shell/power.js';
test('виджет сети', () => {
  assert.equal(mbps(null), '—');
  assert.equal(mbps(125000), '1,00 Мбит/с');
  assert.equal(mbps(12_500_000 / 8 * 8 * 1.5), '150 Мбит/с');
  assert.equal(mbps(1_562_500), '12,5 Мбит/с');
  const m = { net_rx_bytes_per_sec: 30, net_tx_bytes_per_sec: 40, net_by_iface: [{ name: 'eth0', rx_bytes_per_sec: 10, tx_bytes_per_sec: 20 }, { name: 'eth1', rx_bytes_per_sec: null, tx_bytes_per_sec: null }] };
  assert.deepEqual(rates(m, ''), { rx: 30, tx: 40 });
  assert.deepEqual(rates(m, 'eth0'), { rx: 10, tx: 20 });
  assert.deepEqual(rates(m, 'eth1'), { rx: null, tx: null });
  assert.deepEqual(rates(m, 'nope'), { rx: null, tx: null });
  assert.deepEqual(rates(null, ''), { rx: null, tx: null });
  let h = [];
  for (let i = 0; i < HISTORY + 5; i++) h = push(h, i);
  assert.equal(h.length, HISTORY);
  assert.equal(h[HISTORY - 1], HISTORY + 4);
  assert.equal(line([], 1, 100, 30), '');
  const pts = line([0, 10], 10, 100, 30).split(' ');
  assert.equal(pts.length, 2);
  assert.equal(pts[1], '100.0,1.0'); // максимум — вверху справа
  assert.equal(line([null], 0, 100, 30), '100.0,29.0');
});
test('ссылка на событие журнала', () => {
  assert.equal(eventParam('#/journal?event=42'), 42);
  assert.equal(eventParam('#/journal'), 0);
  assert.equal(eventParam('#/journal?event=x'), 0);
  assert.equal(eventParam('#/journal?event=-3'), 0);
});

test('подписи тишины уведомлений', () => {
  assert.equal(notifyText({ type: 'notify.quiet_started', data: { reason: 'работы' } }), 'Включена тишина уведомлений: работы');
  assert.equal(notifyText({ type: 'notify.quiet_ended', data: { suppressed: 4 } }), 'Тишина уведомлений закончилась, не отправлено: 4');
});
