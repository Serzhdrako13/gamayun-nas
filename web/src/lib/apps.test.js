// Помощники раздела «Приложения».
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { stateKind, subKind, portUrl, ready, requestBody, validId, waitTask } from './apps.js';

test('значки состояний', () => {
  assert.equal(stateKind('running'), 'ok');
  assert.equal(stateKind('crashing'), 'bad');
  assert.equal(stateKind('waiting_pool'), 'warn');
  assert.equal(stateKind('???'), '');
  assert.equal(subKind('running'), 'ok');
});

test('ссылка на порт: только tcp и «в сети»', () => {
  assert.equal(portUrl({ host: 8080, container: 80, proto: 'tcp', network: true }, 'nas'), 'http://nas:8080/');
  assert.equal(portUrl({ host: 8443, container: 443, proto: 'tcp', network: true }, 'nas'), 'https://nas:8443/');
  assert.equal(portUrl({ host: 8080, container: 80, proto: 'tcp', network: false }, 'nas'), '');
  assert.equal(portUrl({ host: 53, container: 53, proto: 'udp', network: true }, 'nas'), '');
});

test('готовность: запреты, конфликты, неподтверждённые риски', () => {
  assert.equal(ready(null, new Set()), false);
  assert.equal(ready({ forbidden: [{}], conflicts: [], unconfirmed: [] }, new Set()), false);
  assert.equal(ready({ forbidden: [], conflicts: [{}], unconfirmed: [] }, new Set()), false);
  const c = { forbidden: [], conflicts: [], unconfirmed: ['privileged:a', 'network_port:80/tcp'] };
  assert.equal(ready(c, new Set(['privileged:a'])), false);
  assert.equal(ready(c, new Set(['privileged:a', 'network_port:80/tcp'])), true);
});

test('тело запроса: env null при «оставить», только отмеченные риски', () => {
  const b = requestBody({ id: 'web', name: 'W', compose: 'x', env: 'A=1', keepEnv: true },
    { risks: [{ key: 'a' }, { key: 'b' }] }, new Set(['b', 'zzz']), new Set(['8080/tcp']));
  assert.deepEqual(b, { id: 'web', name: 'W', compose: 'x', env: null, network: ['8080/tcp'], confirm: ['b'] });
  assert.equal(requestBody({ env: 'A=1', keepEnv: false }, null, new Set(), new Set()).env, 'A=1');
});

test('идентификатор', () => {
  for (const s of ['web', 'my-app2']) assert.ok(validId(s), s);
  for (const s of ['', 'Web', '1a', 'a-', 'a_b', 'a'.repeat(33)]) assert.ok(!validId(s), s);
});

test('ожидание задачи', async () => {
  const states = ['running', 'succeeded'];
  const tk = await waitTask(async () => ({ id: 't', state: states.shift() }), { id: 't', state: 'queued' }, 10000, async () => {});
  assert.equal(tk.state, 'succeeded');
});

// Доступ к общим папкам.
import { shareRows, fillGrants, usedGrants } from './apps.js';

test('shareRows: по папке, readOnly — только если все bind :ro', () => {
  const check = { share_binds: [
    { service: 'a', share: 'Photos', target: '/p', read_only: true, grant: '' },
    { service: 'b', share: 'Photos', target: '/q', read_only: false, grant: '' },
    { service: 'a', share: 'Media', target: '/m', read_only: true, grant: 'ro' },
  ] };
  assert.deepEqual(shareRows(check), [
    { share: 'Media', services: ['a'], readOnly: true },
    { share: 'Photos', services: ['a', 'b'], readOnly: false },
  ]);
  assert.deepEqual(shareRows(null), []);
});

test('fillGrants: по умолчанию «только чтение», при переводе — как было', () => {
  const check = { share_binds: [{ service: 'a', share: 'Photos', read_only: false }, { service: 'a', share: 'Docs', read_only: true }] };
  assert.deepEqual(fillGrants({}, check), { grants: { Photos: 'ro', Docs: 'ro' }, added: true });
  assert.deepEqual(fillGrants({}, check, true).grants, { Photos: 'rw', Docs: 'ro' });
  assert.deepEqual(fillGrants({ Photos: 'rw', Docs: 'ro' }, check), { grants: { Photos: 'rw', Docs: 'ro' }, added: false });
});

test('usedGrants и requestBody с доступом', () => {
  const check = { share_binds: [{ service: 'a', share: 'Photos', read_only: false }] };
  assert.deepEqual(usedGrants({ Photos: 'rw', Old: 'ro' }, check), { Photos: 'rw' });
  const b = requestBody({ id: 'x', name: 'x', compose: 'c', env: '', keepEnv: false }, null, new Set(), new Set(),
    { runAs: { a: 'puid' }, grants: { Photos: 'ro' }, migrate: true });
  assert.deepEqual([b.run_as, b.grants, b.migrate], [{ a: 'puid' }, { Photos: 'ro' }, true]);
  assert.equal(requestBody({ id: 'x', name: 'x', compose: 'c', env: '' }, null, new Set(), new Set()).grants, undefined);
});
