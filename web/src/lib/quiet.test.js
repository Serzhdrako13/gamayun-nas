// Срок и подпись тишины уведомлений.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { quietUntil, quietText } from './quiet.js';

test('срок тишины', () => {
  const now = new Date(2026, 10, 18, 22, 30);
  assert.equal(quietUntil('1h', now).getTime(), now.getTime() + 3600e3);
  assert.equal(quietUntil('4h', now).getTime(), now.getTime() + 4 * 3600e3);
  const tm = quietUntil('tomorrow', now);
  assert.deepEqual([tm.getDate(), tm.getHours(), tm.getMinutes()], [19, 9, 0]);
  assert.equal(quietUntil('custom', now, '2026-11-19T01:00').getHours(), 1);
  assert.equal(quietUntil('custom', now, ''), null);
  assert.equal(quietUntil('custom', now, '2026-11-18T10:00'), null, 'прошлое');
  assert.equal(quietUntil('custom', now, '2026-12-01T10:00'), null, 'дольше 7 суток');
});

test('подпись тишины', () => {
  assert.match(quietText(null), /как обычно/);
  const s = quietText({ until: 'x', reason: 'замена диска', mute_critical: true, suppressed: 3, suppressed_critical: 1 }, () => 'T');
  assert.equal(s, 'Тишина до T: замена диска. Критичные тоже не отправляются. Не отправлено: 3, из них критичных: 1.');
});
