# Read-only pool

Sometimes the system itself forbids writing to a pool: you can open files and copy them off, but not
create, change or delete them. It does this when **redundancy is exhausted** — one more disk failure
would cause data loss. Redundancy is a spare copy of the data on other pool disks. It is like
stopping loading cargo into a boat that has already taken on water: first save what you have.

There is also a second case — an **emergency** one: a write to disk failed, and the pool file system
stopped accepting writes by itself so as not to damage what is already written. See the chapter
“Emergency read-only (write error)” below.

## What you see
- The banner “The pool is read-only: redundancy is exhausted” or a message about a specific RAID array
  of the pool (“… degraded (a disk dropped out): one more disk failure will cause data loss”). It
  appears on the home page, in the pool list and on the pool card.
- On the pool card, the **Writing to the pool** block — an explanation, the **Reason (required)** and
  **Duration** fields, and the **Allow writing for this time** button.
- Shared folders on this pool are read-only over the network too.
- If writing was allowed manually — the banner “Writing temporarily allowed manually until …”; the
  **Writing to the pool** block shows who allowed it, with what reason and until when.
- Below — the **Protection log**: who switched the pool to read-only or allowed writing, and when.

## How it works
1. The system constantly watches the pool arrays. As soon as redundancy is exhausted, the pool is
   remounted read-only — without your involvement.
2. When redundancy is restored (for example, after a rebuild), writing comes back by itself.
3. A manual write permission lasts until the end of the chosen duration. If redundancy is not
   restored by then, the pool becomes read-only again.

## Emergency read-only (write error)
If a pool disk failed during a write, the pool file system switches to read-only by itself. The disks
and arrays may still look healthy — the failure happened at the moment of writing.
- The pool state is **Read-only (write error)**, and the pool is listed as needing attention on the
  home page.
- The banner “The pool file system switched to read-only after a write error”.
- On the pool card, the **What is happening** block contains a line saying Btrfs switched to read-only
  after a write error.
- In the event log and notifications — “The pool switched to read-only after a write error”, and after
  recovery — “The pool is writable again”.
- **Allow writing for this time** is not offered here: the restriction was not set by protection, so
  the button would not help.

What to do:
1. **Copy off important data** — reading works.
2. Check the disks: the “Disks” tab, SMART data, the event log.
3. This mode does not clear by itself: the pool has to be remounted (its file system unmounted and
   mounted again). There is no button for this yet — contact the server administrator. If a disk is
   faulty, replace it first, otherwise writing will be disabled again.

## What to do
1. **Copy off important data** — reading works. This is the first step.
2. Find the cause — usually a [disk that dropped out](help:storage.pools.incomplete).
3. If you urgently need to write (for example, a program cannot work with a read-only folder): in the
   **Writing to the pool** block enter a reason, choose a duration (from **15 minutes** to **7 days
   (maximum)**) and click **Allow writing for this time**. If another disk fails, data written during
   this time may be lost.
4. When done, click **Revoke permission now** without waiting for the period to end.

## Turn protection off (warn only)
Sometimes stopping writes does more harm than the risk: a service or program urgently needs to finish
writing or restoring data, and with a read-only pool it is stuck. For this case, protection can be
turned off. It is like keeping going in a boat that has taken on water because you need to reach the
shore now.

The risk: if redundancy is exhausted and one more disk fails, **the pool data will be lost** — both
the old data and everything written after the disk dropped out.

1. The pool card, the **Protection when redundancy is exhausted** block.
2. Choose **Warn only**. A risk warning and the **Reason (required, recorded in the log)** field appear.
3. Enter the reason and click **I understand the risk, turn protection off**. If the pool was already
   read-only because of protection, writing comes back immediately.
4. While protection is off, a “Pool protection is off…” banner stays on the home page and on the pool
   card. If redundancy is exhausted, the banner is red, and the event log and notifications show
   “Redundancy exhausted while pool protection is off — the pool stays writable”.
5. When the urgent work is done, choose **Switch to read-only (recommended)**, enter a reason and click
   **Turn protection on**. If redundancy is still exhausted, the pool switches to read-only immediately.

Who turned protection off and on, when and why — see the **Protection log**.

The switch does not help with the **emergency** read-only mode (write error): that was set by the file
system itself, not by protection. See the chapter above.

If you need to write only briefly, **Allow writing for this time** is better: the permission is
revoked by itself.

## What not to do
- Do not keep writing allowed “just in case”: protection turned on precisely so that new files do not
  land on a pool that may lose data.
- Do not leave protection off after the urgent work: the banner on the home page reminds you.
- Do not try to “fix” the pool by deleting and re-creating it: **Delete pool** destroys all data
  irreversibly.

See also: [Pool degraded (disk dropped out)](help:storage.pools.incomplete), [Pools and RAID levels](help:storage.pools.raid).

:::details For administrators
Protection remounts the pool file system (`mount -o remount,ro`) and makes the pool's shared folders
read-only; the check runs on a timer and immediately when `/proc/mdstat` changes. The emergency mode is
set by the kernel after a Btrfs write error: details — `journalctl -k | grep -i btrfs`. It is cleared
only by unmounting and mounting the pool file system again; there is no button for this yet.
:::
