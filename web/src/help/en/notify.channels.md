# Notification channels

Channels are where the NAS sends notifications besides the notification center: **Email (SMTP)** and
**webhook** (a request to your URL — for example, a chat bot or a monitoring system). Warnings and
critical events are sent.

## Email (SMTP)
- **Send emails** — turn the channel on.
- **Server**, **Port**, **Security**: **STARTTLS (usually port 587)**, **Implicit TLS (usually port 465)**
  or **no encryption** — only for a local server without a password.
- **Login** and **Password**; a saved password is not shown — leave the field empty to keep it, or
  click **Delete password**.
- **Sender** and **Recipients (comma-separated)**.
- **Server certificate**: your own trusted certificate (PEM) for a mail server with a self-signed
  certificate. **Do not verify certificate** is insecure: a man in the middle can intercept emails and
  the password.
- **Send test email** — check the settings. “The server accepted the message” does not yet mean the
  email was read or did not land in spam.

## Webhook
- **Send requests**, **URL (https://)**.
- **HMAC signing key** — lets the receiver verify that the request came from your NAS.
- **Allow http://** — no encryption; a signing key is not allowed in this case.
- **Send test request**. Success is a 2xx response; redirects are not followed.

Changes take effect after **Save**. Below each channel you see the last delivery time, the queue and
the **Last error**.

## How it works
- A series of identical warnings (same type and subject) within 5 minutes is sent as one message right
  away and a summary with the repeat count at the end of the window; critical ones are always sent at once.
- Sending can be muted during maintenance: [Notifications](help:notify), the “Quiet period during
  maintenance” block.
- The notification center is the primary place: if a channel is unavailable, the notification is still there.

See also: [Notifications](help:notify).

:::details For administrators
A webhook is a POST request with a JSON body. With a signing key, the `X-Gamayun-Timestamp` and
`X-Gamayun-Signature: sha256=…` headers are added — HMAC-SHA256 of the string “timestamp.body”.
:::
