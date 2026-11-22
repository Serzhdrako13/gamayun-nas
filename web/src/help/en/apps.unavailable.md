# Apps unavailable, paused or pool busy

Apps keep everything on the pool. If something is wrong with the pool, the NAS stops Docker so that
apps do not write "into the void" and damage data.

## What you see
- **unavailable** — "Apps unavailable: pool … is not connected". The pool is not mounted: for example,
  the disks were not detected after power-on.
- **paused** — "Apps paused: pool … is read-only". The pool stopped accepting writes (a write error or
  [pool protection](help:storage.pools.readonly)).
- **paused** with the text "pool … is writable again — resume manually" — writing is back, but the apps
  wait for your decision.
- **pool busy** — "Docker is stopped, but pool … is busy: process <number> <name>: …". Docker is stopped,
  but another program holds the pool: for example, an SSH or console session opened in a pool folder, a
  program with an open pool file, or a container process that did not exit. While it holds the pool, the
  NAS cannot unmount or remount it. The log gets a single "pool … busy" warning.
- **error** — Docker did not start; the reason is in the banner.

## What to do
1. **Unavailable**: bring the pool back (connect the disks, check the Storage section). When the pool is
   connected again, Docker and the apps start by themselves.
2. **Paused**: deal with the pool first. When writing is back, click **Resume**.
3. **Pool busy**: close the program or session named in the banner (leave the pool folder in the SSH
   session, close the file), then wait up to 15 seconds — the state clears by itself — and click
   **Resume**. If that does not help or the process cannot be found, restart the NAS.
4. **Error**: the NAS retries the start every 5 minutes; **Resume** retries now.

## What not to do
- Do not start Docker manually over SSH while the pool is unavailable: app data would end up on the
  system disk.

See also: [Apps](help:apps), [Pool is read-only](help:storage.pools.readonly).

:::details For administrators
The pool state is checked every 15 seconds; a failed Docker start is retried every 5 minutes. To see
who holds the pool, run on the server: `fuser -vm <pool mount point>` or
`lsof +f -- <pool mount point>`.
:::
