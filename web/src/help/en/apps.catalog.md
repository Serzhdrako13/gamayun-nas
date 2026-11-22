# App catalog

The catalog is a list of apps verified by GamayunNas. Unlike
[your own app from a compose file](help:apps.add), you do not need to write a configuration here:
filling in a short form is enough.

## What "Verified by GamayunNas" means
- **Signature.** The catalog is signed with the project key. The NAS will not accept a forged archive, a
  modified file or a signature by an unknown key — the catalog simply will not load.
- **Pinned images.** Each image is specified with a checksum: exactly the version that was verified is
  downloaded, even if someone replaces it on the internet.
- **Declared risks.** Everything the app needs beyond the usual (device access, special privileges) is
  recorded in the catalog and checked against its settings on installation. An undeclared risk —
  installation is refused.
- **The catalog number only grows.** Updating to an older catalog over the network is impossible; when
  importing a file, the NAS asks for confirmation.

## How to load or update the catalog
- **Refresh catalog** — download a fresh catalog over the network (the button is there if a catalog
  address is set).
- **Import file** — for a NAS without internet: choose the `catalog-<version>.tar.gz` archive.

## Installation
1. Click **Install** on the app card.
2. Fill in the form. Passwords marked "Generated automatically and kept in the app secrets" are created
   by the NAS itself (they are not shown and never get into logs).
3. Choose shared folders if the app asks for them, and the ports to open to the network.
4. **Check** — the NAS checks compatibility (system version, architecture, memory, disk), the signature
   and the risks. Confirm the risks you agree with ([Risks](help:apps.risks)).
5. **Install.** If anything goes wrong at any step or the app does not become ready in time, the
   installation is fully rolled back: no containers, account or empty data remain.

## Update and rollback
- On the app card: **Check for update** → release notes → **Update**.
- Before updating, the NAS takes a **snapshot of the app data**. If the new version does not start or
  does not become ready — **automatic rollback**: the previous version and the data from the snapshot.
  A warning appears in the log.
- **Roll back to …** — return to the previous version manually. Data changed after the update will be
  lost: it is restored from the snapshot. The last three restore points are kept.
- If power is lost in the middle of an update, the NAS completes the rollback on the next start.
- The note "Data migration is irreversible" means the new version converts the data for itself: you can
  only go back together with the data from the snapshot.

## What not to do
- Do not change app files on the pool manually: they are overwritten on update.

See also: [Apps](help:apps), [App states](help:apps.states),
[Folder access](help:apps.access), [App web address](help:apps.web).

:::details For administrators
The catalog is a `catalog-<version>.tar.gz` archive signed with ed25519; trusted keys are built into the
system. Files of an installed app: `<pool>/.apps/projects/<id>/` (secrets are in files with mode 0600),
restore points: `<pool>/.apps/points/<id>/`.
:::
