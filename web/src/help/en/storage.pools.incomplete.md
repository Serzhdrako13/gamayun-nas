# Pool degraded (disk dropped out)

The **Degraded (disk dropped out)** state means that one of the pool disks stopped working as part of
it — it failed, was disconnected or removed. The data is still intact: the system takes what is missing
from the remaining disks. But there is less redundancy now, or perhaps none at all. It is like driving
without a spare tyre: you can get there, but the next puncture is real trouble.

## What you see
- In the pool list and on the pool card — the **Degraded (disk dropped out)** state.
- On the home page — a line “Pool …: Degraded (disk dropped out).” with an **Open** button.
- On the pool card — the **What is happening** block: the system's explanations of what is wrong.
- In **Notifications** — “RAID array degraded: a disk dropped out”; if channels (email, webhook) are
  set up, the same message goes there.
- If redundancy is exhausted — a red banner “The pool is read-only: redundancy is exhausted” or a
  message about a specific RAID array of the pool.

## What is at risk
It depends on the layout ([Pools and RAID levels](help:storage.pools.raid)):

| Layout | After one disk drops out |
|---|---|
| RAID 6, SHR-2 | redundancy remains: survives one more disk failure; writing is allowed |
| RAID 1 with 3+ disks | redundancy remains; writing is allowed |
| RAID 1 with 2 disks, RAID 5, RAID 10, SHR | no redundancy: the next failure means data loss; the pool **switches to read-only** |

Read-only mode turns on automatically so that new files do not end up on a pool that may soon lose
data. You can still read and copy files off.

## What to do
1. **Copy off the most important data** while the pool is readable, to another device. This is always
   the first step when there is no redundancy left.
2. Open **Storage → Disks**: is the dropped-out disk visible, what does SMART show? If the disk came
   loose (cable, drive bay), put it back and check how the pool state on its card changed.
3. If the pool card has an **Unfinished disk operation** block, a disk replacement or addition was
   interrupted. Return or replace the disk and click **Resume replacement**.
4. Replacing a disk with a new one is **not yet available in the web interface** — there is no button.
5. If you urgently need to write — in the **Writing to the pool** block enter a reason and a duration
   (15 minutes … 7 days) and click **Allow writing for this time**. Do this deliberately: if another
   disk fails, the new data may be lost. The permission and the reason are recorded in the log; to end
   it early — **Revoke permission now**.

## What not to do
- Do not count on a **data scrub**: it is only available for a healthy pool, and the **Scrub now**
  button will be inactive.
- Do not pull out other pool disks “to see which one is broken”: for a pool without redundancy this
  means data loss.
- **Delete pool** — only if the data is no longer needed or already copied off: deletion destroys
  everything irreversibly.

See also: [Pools and RAID levels](help:storage.pools.raid), [Read-only pool](help:storage.pools.readonly).

:::details For administrators
Pool RAID array state — `cat /proc/mdstat` and `mdadm --detail /dev/md<N>`. How many failures an array
tolerates: RAID 1 — all disks but one; RAID 5 — 1; RAID 6 — 2; RAID 10 — 1 (counted for the worst case).
For SHR and SHR-2, redundancy is counted per layer, and the result is the smallest one.
:::
