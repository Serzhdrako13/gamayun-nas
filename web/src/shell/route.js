// Адрес оболочки: #/<вид>[?...] — вид из манифестов (sections.js);
// раздел и пункт его меню определяются по виду. #/ — главная.
// Старые адреса (#/pools?open=…) совпадают с ключами видов и работают как раньше.
import { SECTIONS } from './sections.js';

export function parseRoute(hash) {
  const view = (hash || '').replace(/^#?\/?/, '').split('?')[0];
  if (!view) return { section: 'home', nav: 0 };
  for (const s of SECTIONS) {
    if (s.soon) continue;
    if (s.id === view) return { section: s.id, nav: 0 };
    const i = s.nav.findIndex((n) => n.view === view);
    if (i >= 0) return { section: s.id, nav: i };
  }
  return { section: 'home', nav: 0 };
}

export function routeOf(section, nav = 0) {
  if (section === 'home') return '#/';
  const s = SECTIONS.find((x) => x.id === section);
  return '#/' + (s?.nav[nav]?.view || section);
}
