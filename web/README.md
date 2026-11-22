# web — веб-интерфейс GamayunNas

Svelte 5 + Vite 8; версии закреплены в `package.json` и `package-lock.json`.
Сборка кладётся в `../internal/web/dist` и встраивается в бинарник `gamayund` (`go:embed`).
`internal/web/dist` хранится в репозитории: чтобы собрать `gamayund`, Node не нужен.

```sh
cd web
npm ci            # ровно версии из package-lock.json (Node 22+)
npm test          # тесты логики и справки (node --test)
npm run build     # -> ../internal/web/dist
npm run dev       # сервер разработки Vite
```

После изменения `src/` пересоберите и закоммитьте `internal/web/dist`.

Тексты интерфейса — `src/lib/i18n/{ru,en}`, справка — `src/help/{ru,en}/<раздел>.md`.
