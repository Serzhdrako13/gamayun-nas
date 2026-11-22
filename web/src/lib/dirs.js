// Цели переноса вложенной папки. dirs — дерево из
// GET /nas/shares/{имя}/dirs (узлы {name, path, children, partial?};
// children === null — глубже не запрашивалось).
import { t } from './i18n/index.js';

const parentOf = (p) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '');

// Каталоги, куда можно перенести src: все известные, кроме самого src, его
// подкаталогов и текущего родителя; корень ('') — если src не в корне.
// incomplete — список неполный (предел глубины или записей): нужен ввод пути.
export function moveTargets(dirs, src, partial = false) {
  const from = parentOf(src);
  const out = [];
  let incomplete = partial;
  const walk = (nodes) => {
    for (const n of nodes || []) {
      if (n.path === src || n.path.startsWith(src + '/')) continue;
      if (n.path !== from) out.push(n.path);
      if (n.partial) incomplete = true;
      if (n.children == null) incomplete = true;
      else walk(n.children);
    }
  };
  walk(dirs);
  if (from !== '') out.unshift('');
  return { targets: out, incomplete };
}

// Ошибка выбора цели до отправки (дублирует проверку gamayund) или ''.
export function moveError(src, to) {
  if (to === src || to.startsWith(src + '/')) return t('dirs.moveSelf');
  if (to.startsWith('/') || to.endsWith('/') || to.split('/').some((c) => c === '..' || c === '.' || (c === '' && to !== ''))) {
    return t('dirs.movePath');
  }
  if (to === parentOf(src)) return t('dirs.moveSame');
  return '';
}
