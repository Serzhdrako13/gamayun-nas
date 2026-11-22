# Settings

**Settings** holds the server's own parameters: name and time, interface appearance, check schedule,
disk temperature thresholds, log retention, home folders, SSH, sign-in lockouts and the HTTPS
certificate. The **Configuration backup** tab — [Configuration backup](help:settings.confbackup).

## Server name and time
Changing the name restarts the Samba service: open network connections to folders will be interrupted.
The `name.local` address changes along with the name.

## Appearance
Style, theme and language are remembered in this browser.

## Scheduled checks
Defaults: pool data scrub — every 30 days, short SMART test — every 7 days, long — every 30 days;
start — at 3 a.m. server time.
A scrub reads all copies of data on the pool disks and fixes read errors; pools are checked one at a time.
If the server was off at the scheduled time, the missed check runs once at the next scheduled hour.
Problems found are reported as notifications — in “Notifications” and through configured channels.

## Disk temperature
Default thresholds: HDD — 55 °C (warning) and 65 °C (critical), SSD and NVMe — 70 and 80 °C.
SMART is polled every 30 minutes; a sleeping disk is not woken up. Overheating produces an event and a
notification at most once an hour per disk (rising to “critical” — immediately); dropping 3 °C below the
threshold produces a “normal” event. Temperature history is kept for 90 days.

## Log retention
Defaults: regular events and warnings — 90 days, critical — 365 days.
The same periods apply to “Notifications”. Old records are deleted on save, at service start and once
a day; the latest event of each disk and pool is always kept. On top of the periods there is a record
limit: 10,000 events, 5,000 notifications.

## Home folders
Each user gets a home folder `homes/<name>` on the selected pool: the user owns it, others cannot see it.
Over the network it opens as “homes” and serves as the home directory for SSH sign-in. Nothing is
created outside storage (in /home). When turned off, the folders stay untouched on the pool, but users
no longer see them.

## SSH service
After initial setup, SSH is off. Only users with the SSH sign-in right (“Users” section) can sign in,
with their NAS account password; root sign-in is forbidden. Turning SSH off keeps open sessions — the
**Terminate open sessions** option ends them.

## SSH brute-force protection
Off by default; changes are applied with **Save**. When it is turned on, your computer's address is
added to the exceptions automatically so you do not lock yourself out. Only SSH is blocked: signing in
to this interface and network folders are not affected.

## Web interface login lockouts
After a series of wrong passwords, sign-in is temporarily blocked by address and by name (from 1 second
to 15 minutes, growing with the number of attempts). A browser that has already signed in is not
affected by the name lockout. Restarting the server clears all lockouts.

## HTTPS certificate
When you upload your own certificate, the key match, validity period, “server” usage and names are
checked. The certificate takes effect immediately for new connections; it is stored on the server and
the key is never handed out. Compare the SHA-256 fingerprint with what the browser shows.

See also: [Configuration backup](help:settings.confbackup), [Disks](help:storage.disks),
[Log](help:journal), [Users](help:users).
