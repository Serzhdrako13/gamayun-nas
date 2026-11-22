// Выключение и перезагрузка: тексты подтверждения — в словаре
// power, геттеры читают текущий язык при отрисовке.
import { t } from '../lib/i18n/index.js';

const texts = (a) => ({
  get label() { return t(`power.${a}.label`); },
  get q() { return t(`power.${a}.q`); },
  get done() { return t(`power.${a}.done`); },
});
export const POWER = { reboot: texts('reboot'), poweroff: texts('poweroff') };

// eventParam — id события из адреса #/journal?event=<id>; нет — 0.
export function eventParam(hash) {
  const q = (hash || '').split('?')[1] || '';
  const v = Number(new URLSearchParams(q).get('event'));
  return Number.isInteger(v) && v > 0 ? v : 0;
}
