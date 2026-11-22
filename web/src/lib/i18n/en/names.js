// folder and file name errors.
export default {
  what: {
    dir: { nom: 'folder name', gen: 'folder name' },
    file: { nom: 'name', gen: 'name' },
  },
  empty: 'empty {nom}',
  tooLong: '{nom} is longer than {max} bytes',
  dots: 'the names “.” and “..” are not allowed',
  control: 'control character U+{cp} is not allowed in a {gen}',
  forbidden: 'character “{ch}” is not allowed in a {gen} (Windows does not allow < > : " / \\ | ? *)',
  trailingDot: '{nom} cannot end with a dot (Windows drops it)',
  trailingSpace: '{nom} cannot end with a space (Windows drops it)',
  reserved: 'name “{name}” is not allowed: {dev} is a reserved Windows device name (in any case and with any extension)',
};
