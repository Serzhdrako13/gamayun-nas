import { test } from 'node:test';
import assert from 'node:assert/strict';
import { initChoices, buildAnswers, anyKeep, secretsLeftEmpty, toBase64 } from './confbackup.js';

const plan = {
  users: [{ name: 'owner', can_keep: true }, { name: 'ann', can_keep: false }],
  secrets: [{ key: 'smtp_password', title: 'пароль SMTP', can_keep: false }, { key: 'webhook_secret', title: 'ключ webhook', can_keep: true }],
};

test('по умолчанию — новые пароли; пустые — ошибки', () => {
  const ch = initChoices(plan);
  assert.equal(ch.users.owner.mode, 'new');
  assert.equal(anyKeep(ch), false);
  const { errors } = buildAnswers(plan, ch);
  assert.equal(errors.length, 2);
  assert.deepEqual(secretsLeftEmpty(plan, ch), ['пароль SMTP', 'ключ webhook']);
});

test('новые пароли и секреты', () => {
  const ch = initChoices(plan);
  ch.users.owner = { mode: 'new', pw: 'correct horse', pw2: 'correct horse' };
  ch.users.ann = { mode: 'new', pw: 'another pass', pw2: 'another pass' };
  ch.secrets.smtp_password.value = 'x';
  const { answers, errors } = buildAnswers(plan, ch);
  assert.deepEqual(errors, []);
  assert.deepEqual(answers.passwords, { owner: 'correct horse', ann: 'another pass' });
  assert.equal(answers.smtp_password, 'x');
  assert.equal(answers.webhook_secret, '');
  assert.equal(answers.accept_insecure, false);
});

test('несовпадение паролей', () => {
  const ch = initChoices(plan);
  ch.users.owner = { mode: 'new', pw: 'correct horse', pw2: 'correct hors' };
  assert.ok(buildAnswers(plan, ch).errors.some((e) => e.includes('не совпадают')));
});

test('оставить прежний — только где есть и с согласием', () => {
  const ch = initChoices(plan);
  ch.users.owner.mode = 'keep';
  ch.users.ann = { mode: 'keep' };
  ch.secrets.webhook_secret.mode = 'keep';
  let r = buildAnswers(plan, ch);
  assert.ok(r.errors.some((e) => e.startsWith('ann:')));
  assert.ok(r.errors.some((e) => e.includes('небезопасно')));
  ch.users.ann = { mode: 'new', pw: 'another pass', pw2: 'another pass' };
  ch.acceptInsecure = true;
  r = buildAnswers(plan, ch);
  assert.deepEqual(r.errors, []);
  assert.deepEqual(r.answers.keep, { owner: true, webhook_secret: true });
  assert.equal(r.answers.accept_insecure, true);
});

test('base64', () => {
  assert.equal(toBase64(new Uint8Array([0x1f, 0x8b, 0]).buffer), 'H4sA');
});
