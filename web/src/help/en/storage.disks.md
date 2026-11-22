# Disks

The **Disks** tab lists all NAS disks and their health. Like a car, a disk shows signs of wear in
advance: the number of bad sectors grows, the temperature rises. The NAS reads these signs through
**SMART** — the self-diagnostics built into the disk — and warns you while the data is still intact.

## What you see
- **Filter** — search by name, model, ID.
- Table: **Disk**, **Type** (HDD, SSD, NVMe…), **Capacity**, **Model**, **State**, **SMART**.
- **State** — the disk role: **Free** (can be used for a pool), **System** (the system itself lives on
  it), **In use** (already in a pool or otherwise used), **No persistent ID**.
- **SMART** — the result of the last scheduled poll: **normal**, **warning**, **failing**, **asleep**,
  **no data**, **no SMART**, **SMART off**. Hover over the badge to see the reason.
- The banner “The disk list is incomplete” — the system could not determine the role of some disks.
  Such disks may be in use, so they are not offered for pools.

## Disk card
Click a disk name:
- **ID** — the persistent name of the disk. Without it, destructive operations on the disk are not
  allowed and SMART is not polled.
- **System roles** — root, boot, mounted, in a pool; **Why the role is unknown** — if the system is
  not sure.
- **SMART**: **Self-assessment** (normal / failing), **Temperature / power-on time**, **Sectors:
  reallocated / pending / uncorrectable**, **Self-test** — the last result.
- **History** — temperature and bad sector charts; **Period** — 24 h, 7, 30 or 90 days.

## How it works
- SMART is polled every 30 minutes. A sleeping disk is not woken up — the card says the disk is asleep.
- **Failing** — the disk itself reported failure, the last self-test ended with an error, or the
  temperature reached the critical threshold. **Warning** — bad sectors appeared or the temperature
  is above the warning threshold.
- **Temperature.** Default thresholds: HDD — 55 °C warning and 65 °C critical; SSD and NVMe — 70 and
  80 °C. Change them in **Settings → Disk temperature**. An overheating notification is sent at most
  once an hour per disk (a rise to critical — immediately); “normal” — once the temperature drops
  3 °C below the threshold. History is kept for 90 days.
- **Self-tests.** **Short test** — about 2 minutes, does not interfere with work. **Long test** — the
  disk reads its whole surface itself, which takes hours. On schedule, the short test runs every 7
  days and the long one every 30 days, at 3 a.m.; the schedule is in **Settings → Scheduled checks**.

## What to do
- **Failing** or a growing number of bad sectors on a pool disk — copy off important data and prepare
  to replace the disk. While the pool is intact, this is cheaper than after a failure.
- **Overheating** — check the ventilation and dust in the case.
- **SMART off** — click **Turn on SMART**, otherwise the NAS does not monitor the disk.
- **No SMART** — typical for virtual disks and USB adapters; the NAS cannot monitor such a disk.

## What not to do
- Do not turn SMART off without a reason (**Turn off SMART**): the NAS will stop monitoring the disk,
  and SMART will not be turned on automatically.

See also: [Storage](help:storage), [Pool degraded (disk dropped out)](help:storage.pools.incomplete),
[Settings](help:settings).
