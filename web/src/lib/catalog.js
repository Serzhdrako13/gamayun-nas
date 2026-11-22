// Помощники раздела «Каталог» (без Svelte — тестируются в node).

// Текст на языке интерфейса (ru|en), иначе — другой язык.
export const textOf = (x, l) => (x ? x[l] || x.ru || x.en || '' : '');

// Сравнение версий x.y.z[-pre]: <0, 0, >0 (как catalog.CompareVersion).
export function compareVersion(a, b) {
  const split = (v) => {
    const [main, pre = ''] = String(v || '').split('-', 2);
    return { n: main.split('.').map((x) => parseInt(x, 10) || 0), pre };
  };
  const x = split(a);
  const y = split(b);
  for (let i = 0; i < 3; i++) {
    const d = (x.n[i] || 0) - (y.n[i] || 0);
    if (d) return d < 0 ? -1 : 1;
  }
  if (x.pre === y.pre) return 0;
  if (!x.pre) return 1;
  if (!y.pre) return -1;
  return x.pre < y.pre ? -1 : 1;
}

// Поле формы показано: show_if "KEY" (bool true) или "KEY=значение" (как catalog.visible).
export function visibleField(f, values) {
  if (!f.show_if) return true;
  const i = f.show_if.indexOf('=');
  if (i < 0) return values[f.show_if] === 'true';
  return values[f.show_if.slice(0, i)] === f.show_if.slice(i + 1);
}

// Начальные значения формы: default, bool — "false"; random — пусто (сгенерирует NAS).
export function initialValues(form) {
  const v = {};
  for (const f of form || []) {
    if (f.type === 'random') continue;
    v[f.key] = f.default ?? (f.type === 'bool' ? 'false' : '');
  }
  return v;
}

// Значения для отправки: только видимые поля, пустые не отправляются (NAS подставит default/random).
export function formValues(form, values) {
  const out = {};
  for (const f of form || []) {
    if (!visibleField(f, values)) continue;
    const v = values[f.key];
    if (v !== undefined && v !== '') out[f.key] = String(v);
  }
  return out;
}

// Обязательные видимые поля заполнены (кроме random — его сгенерирует NAS).
export function formFilled(form, values) {
  return (form || []).every((f) => !f.required || f.type === 'random' || !visibleField(f, values) || (values[f.key] ?? '') !== '');
}

// Установленные из каталога: id приложения каталога → [приложения NAS].
export function installedOf(projects) {
  const m = {};
  for (const p of projects || []) {
    if (!p.catalog || p.state === 'removed') continue;
    (m[p.catalog.app] ||= []).push(p);
  }
  return m;
}

// В каталоге есть версия новее установленной.
export const hasUpdate = (project, entry) => !!(project?.catalog && entry && compareVersion(entry.manifest.version, project.catalog.version) > 0);

// Порты «в сеть» по умолчанию для установки: exposable из манифеста → "порт/протокол".
export const exposable = (m) => (m?.ports || []).filter((p) => p.exposable).map((p) => ({ ...p, key: `${p.default_host}/${p.protocol || 'tcp'}` }));

