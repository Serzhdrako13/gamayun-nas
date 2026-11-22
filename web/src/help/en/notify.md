# Notifications

Notifications are important events the NAS wants to tell you about: a disk failing or overheating, a
disk dropping out of a pool, a pool switching to read-only, app failures. The log keeps everything,
while notifications are what is worth looking at.

## What you see
- The **Center** tab — the notification list. The **unread only** filter, **Mark all as read**.
  The unread count is shown at the top of the interface.
- The **Channels** tab — sending notifications by email and webhook: [Notification channels](help:notify.channels).
- **Quiet period during maintenance** — temporarily stop sending notifications outside.

## Quiet period during maintenance
Useful, for example, while replacing a disk, so your inbox is not flooded with expected messages.
1. Pick the duration: **1 hour**, **4 hours**, **until tomorrow, 09:00** or **custom** (at most 7 days).
2. Enter a **Reason** and click **Start quiet period**.
3. To end it early — **Cancel quiet period**; to extend it — **Change end time**.

Events are still written to the log and the notification center — only email and webhook stay silent.
By default, critical notifications are sent even during a quiet period. The **do not send critical ones
either** option silences them too — turn it on only if someone is watching the server on site. When the
quiet period ends, one summary is sent with the number of messages not sent.

See also: [Notification channels](help:notify.channels), [Log](help:journal).
