// справка — разбор текстов без зависимостей и без Vite (тесты в node).
// Раздел — файл help/<язык>/<id>.md, первая строка «# » — заголовок.
// Нет раздела на языке интерфейса — показывается ru. Ссылки — [текст](help:id).

export const FALLBACK = 'ru';

// Порядок оглавления: по первому сегменту id, родитель раньше детей.
const TOP = ['overview', 'storage', 'shares', 'files', 'users', 'journal', 'notify', 'settings', 'apps', 'glossary'];

// Все id разделов справки (написанные и запланированные).
export const PLANNED = [
  'overview', 'glossary',
  'storage', 'storage.pools', 'storage.pool', 'storage.pools.raid', 'storage.pools.create',
  'storage.pools.incomplete', 'storage.pools.failed', 'storage.pools.missing', 'storage.pools.unknown',
  'storage.pools.readonly', 'storage.pools.pending', 'storage.pools.scrub', 'storage.pools.found', 'storage.pools.integrity', 'storage.disks',
  'shares', 'files', 'users', 'journal', 'notify', 'notify.channels', 'settings', 'settings.confbackup',
  'storage.tasks.failed',
  'apps', 'apps.add', 'apps.risks', 'apps.ports', 'apps.states', 'apps.unavailable', 'apps.access', 'apps.web', 'apps.catalog',
];

// parseSection — {id, title, body} из текста файла.
export function parseSection(id, raw) {
  const lines = String(raw).replace(/\r\n/g, '\n').split('\n');
  const head = lines.findIndex((l) => l.startsWith('# '));
  const title = head >= 0 ? lines[head].slice(2).trim() : id;
  const body = head >= 0 ? lines.slice(head + 1).join('\n') : String(raw);
  return { id, title, body };
}

// buildIndex — {язык: {id: раздел}} из {путь: текст}; путь …/<язык>/<id>.md.
export function buildIndex(files) {
  const idx = {};
  for (const [path, raw] of Object.entries(files)) {
    const m = path.match(/([a-z]{2})\/([a-z0-9.-]+)\.md$/);
    if (!m) continue;
    (idx[m[1]] ??= {})[m[2]] = parseSection(m[2], raw);
  }
  return idx;
}

export function sectionIn(idx, id, lang) {
  return idx[lang]?.[id] ?? idx[FALLBACK]?.[id] ?? null;
}

function order(id) {
  const i = TOP.indexOf(id.split('.')[0]);
  return i < 0 ? TOP.length : i;
}

// sectionsIn — все разделы (любой язык) в порядке оглавления.
export function sectionsIn(idx, lang) {
  const ids = new Set();
  for (const l of Object.values(idx)) Object.keys(l).forEach((id) => ids.add(id));
  return [...ids]
    .sort((a, b) => order(a) - order(b) || a.localeCompare(b))
    .map((id) => sectionIn(idx, id, lang))
    .filter(Boolean);
}

// searchIn — подстрока в заголовке или тексте, без учёта регистра.
export function searchIn(idx, query, lang) {
  const q = String(query || '').trim().toLowerCase();
  if (!q) return [];
  return sectionsIn(idx, lang).filter((s) => s.title.toLowerCase().includes(q) || s.body.toLowerCase().includes(q));
}

// helpLinks — id из ссылок [текст](help:id) в тексте.
export function helpLinks(text) {
  return [...String(text).matchAll(/\]\(help:([^)\s]+)\)/g)].map((m) => m[1]);
}

// inline — строка → [{t:'text'|'b'|'code'|'help'|'link', v, href?}].
export function inline(text) {
  const out = [];
  const re = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\([^)\s]+\))/g;
  let last = 0;
  let m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push({ t: 'text', v: text.slice(last, m.index) });
    const tok = m[0];
    if (tok.startsWith('**')) out.push({ t: 'b', v: tok.slice(2, -2) });
    else if (tok.startsWith('`')) out.push({ t: 'code', v: tok.slice(1, -1) });
    else {
      const [, v, href] = tok.match(/^\[([^\]]+)\]\(([^)\s]+)\)$/);
      if (href.startsWith('help:')) out.push({ t: 'help', v, href: href.slice(5) });
 // Внешние ссылки — только http(s): иначе показывается текст.
      else if (/^https?:\/\//.test(href)) out.push({ t: 'link', v, href });
      else out.push({ t: 'text', v });
    }
    last = m.index + tok.length;
  }
  if (last < text.length) out.push({ t: 'text', v: text.slice(last) });
  return out;
}

const LIST = /^\s*([-*]|\d+\.)\s/;

// blocks — Markdown → [{t:'h3'|'h4'|'p'|'ul'|'ol'|'table'|'pre'|'details', …}].
// Поддерживается ограниченное подмножество Markdown; HTML не вставляется.
export function blocks(source) {
  const lines = String(source).replace(/\r\n/g, '\n').split('\n');
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    if (line.startsWith(':::details')) {
      const summary = line.slice(':::details'.length).trim();
      const inner = [];
      i++;
      while (i < lines.length && lines[i].trim() !== ':::') inner.push(lines[i++]);
      i++;
      out.push({ t: 'details', summary, children: blocks(inner.join('\n')) });
      continue;
    }
    if (line.startsWith('```')) {
      const code = [];
      i++;
      while (i < lines.length && !lines[i].startsWith('```')) code.push(lines[i++]);
      i++;
      out.push({ t: 'pre', v: code.join('\n') });
      continue;
    }
    if (line.startsWith('### ')) {
      out.push({ t: 'h4', v: inline(line.slice(4)) });
      i++;
      continue;
    }
    if (line.startsWith('## ')) {
      out.push({ t: 'h3', v: inline(line.slice(3)) });
      i++;
      continue;
    }
    if (LIST.test(line)) {
      const ordered = /^\s*\d+\./.test(line);
      const items = [];
      while (i < lines.length && LIST.test(lines[i]) && /^\s*\d+\./.test(lines[i]) === ordered) {
        let item = lines[i].replace(LIST, '');
        i++;
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !LIST.test(lines[i])) item += ' ' + lines[i++].trim();
        items.push(inline(item));
      }
      out.push({ t: ordered ? 'ol' : 'ul', items });
      continue;
    }
    if (line.startsWith('|')) {
      const rows = [];
      while (i < lines.length && lines[i].startsWith('|')) {
        const cells = lines[i].trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
        if (!cells.every((c) => /^:?-+:?$/.test(c))) rows.push(cells.map(inline));
        i++;
      }
      const [head = [], ...body] = rows;
      out.push({ t: 'table', head, body });
      continue;
    }
    const para = [];
    while (i < lines.length && lines[i].trim() && !/^(#{2,3} |\||```|:::|\s*([-*]|\d+\.)\s)/.test(lines[i])) para.push(lines[i++].trim());
    if (!para.length) {
 // Строка, которую ничто не разобрало (например, «# » в теле), — абзацем.
      para.push(lines[i++].trim());
    }
    out.push({ t: 'p', v: inline(para.join(' ')) });
  }
  return out;
}
