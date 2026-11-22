# Files

**Files** is a file manager right in the browser. It is handy when there is no computer with the
network folder connected: for example, to upload photos from someone else's laptop or download a
document to your phone. You see the same shared folders with the same permissions as over the network (SMB).

## What you see
- **Folder** — choose a shared folder. Only folders you have access to are listed; those marked
  **(read)** are read-only. **Home folder** appears if home folders are enabled in **Settings**.
- The path bar — click any part to go up. **Refresh** rereads the folder.
- The table: name, **Size**, **Modified**. Tick rows with the checkboxes (**Select all** selects everything).
- Buttons above the table:
  - **Upload…** — pick files on your computer; you can also just drag them into the window;
  - **New folder**, **Rename** (one selected row);
  - **Download** — each selected file downloads separately, a folder downloads as a zip archive;
  - **Copy…**, **Move…** — choose the destination folder and subfolder (empty — root);
  - **Delete** (or **Delete (N)** for several items).
- The **if the name exists — add a number** checkbox: if the name is taken, the new file is saved as
  “name (2)”. Without it, a name clash is an error; the existing file is not overwritten.
- In a read-only folder you can only browse, **Download** and **Copy…** to another folder where you
  have write access.

## How it works
1. Permissions are the same as over the network: read access lets you browse and download; write access
   also lets you upload, create, rename, move and delete. Permissions are granted in
   [Shared folders](help:shares).
2. Uploads go file by file, each with its own progress and a **Cancel** button. The result is
   “Uploaded: N” or “Uploaded N of M” with a reason for the failed ones (name taken, not enough space,
   no connection).
3. Copying, moving and deleting run as a task with a percentage and a **Cancel** button. Cancelling
   stops the task, but what has already been done stays done.
4. Links and special files are shown but cannot be opened from the browser. In a very large folder,
   the first 10,000 entries are shown.

## What not to do
- Deletion is **permanent, there is no recycle bin**. Deleted items can only be restored from a folder
  snapshot, if snapshots are enabled for it ([Shared folders](help:shares)).
- Do not upload large amounts through the browser if you can connect the folder over the network:
  that is faster and more reliable.

See also: [Shared folders](help:shares), [Users](help:users).
