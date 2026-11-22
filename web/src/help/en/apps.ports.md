# Ports

A port is a numbered "door" of the NAS through which a program accepts network connections. Each door
can have only one owner: if the NAS or another app already uses it, a second one cannot take it.

## What you see
The **Ports** tab — a table with **Port**, **Used by**, **Access**:
- **NAS service** — ports of the NAS itself (SSH 22, web interface 80 and 443, SMB 139 and 445,
  NFS 2049, mDNS 5353 and others). They are never given to apps;
- **app** — a port declared in compose, with access **NAS only** or **network**;
- **used on NAS** — the port is used by another program.

## How it works
1. By default an app port is available **only on the NAS** (address 127.0.0.1): other devices cannot
   connect to it. This is safe while access through the shared NAS address is not ready.
2. Checking **available on the network** in the wizard opens the port to the whole network. A port
   published by Docker goes **around the NAS firewall** — this is a [risk](help:apps.risks) and must be
   confirmed.
3. When the compose file is checked, used ports are shown in **Port conflicts**; an app with a conflict
   cannot be created.

## What to do
Conflict — change the number on the left in compose: `8081:80` instead of `8080:80` (left — the NAS port,
right — the port inside the container), then click **Check**.

See also: [Add app](help:apps.add).

:::details For administrators
The NAS determines used ports from `/proc/net/tcp`, `tcp6`, `udp`, `udp6`. Docker publishes ports with
its own nftables rules, bypassing the NAS firewall input chain — that is why "available on the network"
is treated as a risk.
:::
