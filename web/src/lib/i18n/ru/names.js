// Ошибки имён папок и файлов. Тексты для папки совпадают с gamayund
// (internal/nas/winname.go, internal/nas/testdata/winnames.json) — менять вместе.
export default {
  what: {
    dir: { nom: 'имя папки', gen: 'имени папки' },
    file: { nom: 'имя', gen: 'имени' },
  },
  empty: 'пустое {nom}',
  tooLong: '{nom} длиннее {max} байт',
  dots: 'имена «.» и «..» недопустимы',
  control: 'управляющий символ U+{cp} в {gen} недопустим',
  forbidden: 'символ «{ch}» в {gen} недопустим (Windows не допускает < > : " / \\ | ? *)',
  trailingDot: '{nom} не может оканчиваться точкой (Windows её отбрасывает)',
  trailingSpace: '{nom} не может оканчиваться пробелом (Windows его отбрасывает)',
  reserved: 'имя «{name}» недопустимо: {dev} — зарезервированное имя устройства Windows (в любом регистре и с расширением)',
};
