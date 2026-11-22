# Found pools

A pool keeps a description of its layout on the disks themselves. After reinstalling the system,
replacing the system disk or moving the disks to another server with GamayunNas, the pool can be
imported back — the data on the disks is not changed.

**How to import**
1. Attach all pool disks and open “Storage” (or the “Pool” step of the setup wizard).
2. In the “Found pools” card, type the pool name to confirm and click **Import**.
3. The pool is assembled and mounted; its type and layout are restored from the disks.

**If not all disks are found.** A pool with redundancy (mirror, RAID 5/6, SHR) can be imported without
one disk — check “Import an incomplete pool (not all disks found)”. It will be in the “Degraded (disk
dropped out)” state, as after a disk failure. Pools without redundancy (Basic, JBOD, RAID 0) cannot be
assembled without all their disks.

**Settings after import.** The pool holds daily configuration backups (users, folders, permissions).
In the setup wizard, choose “Restore from backup” → the backup on the pool. Passwords are not included
in the backup — they are set again.

**When import is not possible.** A pool with the same name already exists; the pool volume is in use;
some pool layers are missing. The disks of a found pool cannot be used for a new pool until it is
imported or wiped.
