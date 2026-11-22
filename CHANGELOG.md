# Changelog / Изменения

## 0.1.0-alpha.1 — 2026-11-22

First public release. / Первый публичный выпуск.

**Storage / Хранилище**
- Pools: Basic, JBOD, RAID 0/1/5/6/10, SHR, SHR-2 on mdadm + LVM + Btrfs, optional dm-integrity.
  Пулы: Basic, JBOD, RAID 0/1/5/6/10, SHR, SHR-2 на mdadm + LVM + Btrfs, контроль целостности (dm-integrity).
- Growing pools, disk replacement (including hot replacement), RAID 1 → 5, RAID 5 → 6, SHR → SHR-2;
  interrupted operations resume after reboot.
  Расширение пулов, замена диска (в том числе горячая), переходы RAID 1 → 5, RAID 5 → 6, SHR → SHR-2;
  прерванные операции продолжаются после перезагрузки.
- Read-only protection when redundancy is exhausted; scheduled scrub; SMART, temperature history, self-tests.
  Защита «только чтение» при исчерпании избыточности; проверка пула по расписанию; SMART, история температуры, самотесты.
- Pool import after reinstalling the OS, with settings restore from the backup on the pool.
  Подхват пулов после переустановки системы и восстановление настроек из копии на пуле.

**Sharing / Общий доступ**
- SMB shared folders with user and group permissions, nested folders, web file manager.
  Общие папки SMB с правами пользователей и групп, вложенные папки, файловый менеджер в браузере.
- Btrfs snapshots (manual and scheduled), Windows "Previous Versions", restore as a copy.
  Снимки Btrfs (вручную и по расписанию), «Предыдущие версии» Windows, восстановление копией.
- Users and groups; SSH/SFTP login per user; SSH service off by default, root login disabled.
  Пользователи и группы; вход по SSH/SFTP по праву пользователя; служба SSH выключена по умолчанию, вход root закрыт.

**Applications (in development) / Приложения (в разработке)**
- Docker on a pool (separate `gamayun-nas-apps` package with pinned Docker, Compose and Caddy),
  risk review, per-app accounts, `https://<app>-<nas>.local/` web addresses, signed app catalog.
  Docker на пуле (отдельный пакет `gamayun-nas-apps` с закреплёнными Docker, Compose и Caddy),
  проверка рисков, учётка для каждого приложения, веб-адреса `https://<приложение>-<nas>.local/`, подписанный каталог.

**Monitoring and maintenance / Наблюдение и обслуживание**
- Event log, notification center, e-mail (SMTP) and webhook delivery, quiet hours.
  Журнал событий, центр уведомлений, доставка на почту (SMTP) и webhook, режим тишины.
- Configuration backup to pools, SFTP, USB or file; restore in the first-run wizard.
  Резервная копия настроек на пулы, SFTP, USB или файлом; восстановление в мастере первого запуска.
- HTTPS by default, custom certificate, sign-in brute-force lockout, fail2ban for SSH.
  HTTPS по умолчанию, свой сертификат, блокировка перебора паролей, fail2ban для SSH.
- Web interface in English and Russian, "Tabs" and "Desktop" layouts, built-in help in every window.
  Веб-интерфейс на английском и русском, стили «Вкладки» и «Рабочий стол», встроенная справка в каждом окне.

**Known limitations / Ограничения**
- Debian 13 amd64 only; upgrades by package, no A/B or automatic rollback; no hot spares.
  Только Debian 13 amd64; обновление пакетом, без A/B и автоотката; нет горячего резерва.
- Server messages are in Russian. / Сообщения сервера — на русском.
- Docker applications are in development; interface and formats may change.
  Приложения Docker — в разработке; интерфейс и формат могут меняться.
