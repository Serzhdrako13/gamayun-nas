# App states

The badge next to an app in the list shows what is happening with it right now.

## What you see
| State | What it means | What to do |
|---|---|---|
| **running** | all containers are running and pass the readiness check | — |
| **running (no readiness check)** | running, but the app cannot report that it is ready | if it does not open, check the log |
| **starting** | containers are starting or restarting | wait |
| **unhealthy** | the container is running but fails the readiness check | open the app, **Container log** |
| **crashing** | the app crashed more than 5 times in 10 minutes and was stopped | fix the cause using the log, then **Start** |
| **error** | the container exited with an error or did not start | the log, then **Start** or **Edit** |
| **stopped** | you stopped the app | **Start** |
| **waiting for pool** | the apps pool is not connected | see [unavailable](help:apps.unavailable) |
| **paused** | the pool became read-only or is held by another program | see [paused](help:apps.unavailable) |
| **removed (data kept)** | no containers, the app data remains | create it again with the same identifier or **Remove** it for good |

## How it works
- **Start**, **Stop**, **Restart** — in the app row. **Start** clears the "crashing" state.
- **Container log** on the app card: choose the **Service** and the number of **Lines** (100–5000),
  click **Refresh**; **Download** saves the log as a file. At most 1 MiB is shown.
- **Remove** — with **Keep app data** checked (default) the data stays; unchecked, it is deleted
  permanently together with the app volumes.

## What not to do
- Do not uncheck **Keep app data** until you have copied what you need: the data is deleted without a
  recycle bin.

See also: [Apps](help:apps).

:::details For administrators
Crash detector: 5 crashes in 10 minutes — the app is stopped. Stopping a container with
`docker restart` does not count as a crash. The container log in the interface is limited to 1 MiB.
:::
