import { test } from 'node:test';
import assert from 'node:assert/strict';
import { textOf, compareVersion, visibleField, initialValues, formValues, formFilled, installedOf, hasUpdate, exposable } from './catalog.js';

test('textOf', () => {
  assert.equal(textOf({ ru: 'Да', en: 'Yes' }, 'en'), 'Yes');
  assert.equal(textOf({ ru: 'Да', en: '' }, 'en'), 'Да');
  assert.equal(textOf(null, 'ru'), '');
});

test('compareVersion', () => {
  assert.equal(compareVersion('1.1.0', '1.0.9'), 1);
  assert.equal(compareVersion('1.0.0', '1.0.0'), 0);
  assert.equal(compareVersion('1.0.0-rc1', '1.0.0'), -1);
  assert.equal(compareVersion('1.10.0', '1.9.0'), 1);
});

const form = [
  { key: 'PASS', type: 'random', secret: true },
  { key: 'SSL', type: 'bool' },
  { key: 'CERT', type: 'text', show_if: 'SSL', required: true },
  { key: 'MODE', type: 'select', options: ['a', 'b'], default: 'a' },
  { key: 'B_OPT', type: 'text', show_if: 'MODE=b' },
  { key: 'MEDIA', type: 'share', required: true },
];

test('форма: видимость, значения, обязательные', () => {
  const v = initialValues(form);
  assert.deepEqual(v, { SSL: 'false', CERT: '', MODE: 'a', B_OPT: '', MEDIA: '' });
  assert.equal(visibleField(form[2], v), false);
  assert.equal(formFilled(form, v), false); // MEDIA
  v.MEDIA = 'Photos';
  assert.equal(formFilled(form, v), true);
  v.SSL = 'true';
  assert.equal(formFilled(form, v), false); // CERT показан и обязателен
  v.CERT = 'x';
  v.B_OPT = 'скрыто';
  assert.deepEqual(formValues(form, v), { SSL: 'true', CERT: 'x', MODE: 'a', MEDIA: 'Photos' });
});

test('установленные и обновления', () => {
  const ps = [
    { id: 'demo', catalog: { app: 'demo', version: '1.0.0' }, state: 'running' },
    { id: 'old', catalog: { app: 'demo', version: '1.1.0' }, state: 'removed' },
    { id: 'mine', state: 'running' },
  ];
  const m = installedOf(ps);
  assert.deepEqual(Object.keys(m), ['demo']);
  assert.equal(m.demo.length, 1);
  assert.equal(hasUpdate(ps[0], { manifest: { version: '1.1.0' } }), true);
  assert.equal(hasUpdate(ps[0], { manifest: { version: '1.0.0' } }), false);
  assert.equal(hasUpdate(ps[2], { manifest: { version: '9.0.0' } }), false);
});

test('exposable', () => {
  const l = exposable({ ports: [{ default_host: 8096, exposable: true }, { default_host: 9, protocol: 'udp', exposable: true }, { default_host: 1 }] });
  assert.deepEqual(l.map((p) => p.key), ['8096/tcp', '9/udp']);
});

