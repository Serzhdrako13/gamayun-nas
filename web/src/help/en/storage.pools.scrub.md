# Pool scrub

A data scrub reads all copies of the data on all pool disks. Read errors are fixed from a good copy,
and you get a notification about what was found. It is like a warehouse audit: better to find a
damaged box now, while there is a spare, than on the day you need it.

## What you see
On the pool card, the **Data scrub** block: **Last completed**, **Scrub in progress**, the **Scrub now**
and **Stop scrub** buttons.

## How it works
- The scrub runs in the background and the pool stays available; on large disks it takes hours.
- On schedule — every 30 days at 3 a.m., one pool at a time. Change the schedule in
  [Settings](help:settings), the **Scheduled checks** block. If the server was off at the scheduled
  time, the scrub runs at the nearest scheduled hour.
- A scrub is only available for a healthy pool and after the initial sync finishes.

See also: [Storage](help:storage), [Pool degraded (disk dropped out)](help:storage.pools.incomplete).
