# Log

The log is the NAS's chronicle: who signed in, what happened to disks and pools, which checks ran,
what apps did. When something goes wrong, look here first: what happened and in what order.

## What you see
- The event list, **newest first**: text, subject (disk, pool, app…) and time.
- The colored dot is the severity: green — info, yellow — warning, red — critical.
- **Show more** loads older events; **Refresh** rereads the list.
- From a notification or the **Events** widget (**Open in Log**), the log opens at the right event and
  highlights it. If the event has already been deleted by retention, you will see “Event #… not found”.

The log has no type filters or search yet. Important events are easier to review in
[Notifications](help:notify): there is an **unread only** filter there.

## What gets logged
- sign-ins and sign-outs, failed sign-ins and lockouts, password changes, user creation;
- disks: added, disconnected, returned, failed, overheating and temperature back to normal;
- pools and RAID arrays: a disk dropped out, recovery, integrity errors, switching to read-only and
  back, pool protection turned off;
- shared folder snapshots and restores from them;
- apps and Docker: start, stop, failures, confirmed risks;
- the SSH service, notification quiet time, server restart and shutdown.

## Retention
Old events are deleted automatically. Retention is set in **Settings → Log retention** separately for
each severity:

| Severity | Default |
|---|---|
| Regular events (info) | 90 days |
| Warnings (warning) | 90 days |
| Critical (critical) | 365 days |

- A period is from 1 to 3650 days (10 years). The same periods apply to notifications.
- Cleanup runs when the periods are saved, when the service starts and once a day.
- On top of the periods there is a limit on the number of records: 10,000 events and 5,000 notifications.
- The latest event of each disk and pool is always kept.

## What not to do
- Do not set periods that are too short: investigating a rare failure (a disk that drops out once a
  month) needs several months of history.

See also: [Notifications](help:notify), [Settings](help:settings).

:::details For administrators
Detailed service logs are on the server: `journalctl -u gamayund` and `journalctl -u gamayun-helper`.
:::
