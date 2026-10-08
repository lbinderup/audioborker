# Running audioborker on a QNAP NAS (Container Station)

This gets audioborker running next to your other *arr apps in Container
Station, pulling a published image the same way they do.

## 1. Publish the image (once)

The repository builds a multi-arch image (amd64 + arm64) in GitHub Actions and
pushes it to GitHub's container registry.

1. Push to `main` — the **Build and publish image** workflow runs automatically.
   Watch it under the repo's **Actions** tab; it takes a few minutes (the arm64
   layer builds under emulation).
2. When it finishes, the image exists at
   `ghcr.io/lbinderup/audioborker:latest`.
3. **Make the package public** so Container Station can pull it without
   credentials: GitHub → your profile → **Packages** → `audioborker` →
   *Package settings* → **Change visibility** → Public.

   Prefer to keep it private? Then in Container Station go to
   **Preferences → Registry → Add**, URL `https://ghcr.io`, username = your
   GitHub username, password = a Personal Access Token with `read:packages`.

> Building on the NAS instead: SSH in, `git clone` the repo, and run
> `docker build -t audioborker:latest .`. It works, but it's slow on NAS
> hardware and needs the CLI — the registry route is easier to keep updated.

## 2. Find your PUID / PGID

audioborker runs as an unprivileged user inside the container. That user must
own (or be able to read/write) your media folders, or conversions fail with
permission errors.

SSH into the NAS and check who owns the media:

```bash
ls -ln /share/Media/Audiobooks     # the numbers in cols 3 and 4 are uid and gid
id your-username                        # uid/gid for a specific account
```

Use those numbers as `PUID`/`PGID`. On QTS the `everyone` group is commonly
`100`; regular users start at `1000`. Don't guess — read them off the share.

## 2b. The two-user problem (and how to end it for good)

Most QNAP *arr setups end up with two identities that can't manage each
other's files:

- your **personal login** (typically uid 1000), which owns anything you copy
  from your desktop over SMB;
- a **service account** used by the containers (often uid 1001), which owns
  everything the *arr apps download and sort.

Each can *read* the other's files but not delete or rename them, so Sonarr
chokes on files you uploaded, and you can't clean up what Sonarr produced.

The cause is that files and folders get created `644`/`755`: only the owner can
write. Ownership differs, so nobody can write to everything.

Note the part that trips people up: **deleting a file requires write permission
on the directory containing it, not on the file.** So an app that creates its
library folders with `755` locks everyone else out of removing or renaming
anything inside, however permissive the files themselves are.

The fix has three parts, and **all three are required** — this is why the usual
"just set PUID" advice doesn't stick.

**1. A shared group both identities belong to.**
Control Panel → Privilege → User Groups → create `media`, then add both your
personal account and the container service account to it. (Or reuse an
existing common group — `everyone` is gid 100.) Note its gid:
`getent group media`.

**2. Apply it to the libraries, with the setgid bit so it sticks.**
Over SSH, per media share:

```bash
LIB=/share/Media/Audiobooks      # adjust to your share
chgrp -R media "$LIB"
chmod -R g+rwX "$LIB"
find "$LIB" -type d -exec chmod g+s {} +
```

`g+rwX` grants group write (capital `X` = execute on directories only).
The **setgid bit** (`g+s`) is the important part: new files and folders created
inside inherit the `media` group instead of the creator's primary group, so
the arrangement survives everything created from then on.

**3. Make every writer create group-writable files — `umask 002`.**
This is the step people miss. A umask can only *remove* permission bits, so
setgid alone still yields 644 files that the group cannot write.

- audioborker: `UMASK=002` (already the default in this image).
- linuxserver.io *arr containers: add `- UMASK=002` to their environment.
- Your SMB uploads: **don't** go looking for a `create mask` setting — QTS has
  no GUI for it, and its generated Samba config sets `inherit permissions = yes`,
  which overrides the mask options anyway. Files copied in from a PC inherit the
  *parent directory's* mode instead. So step 2 above is the whole fix: set the
  tree to `2775` and uploads land `0664` in the right group automatically.

If permissions still don't behave, check the two things that override POSIX:

- **The sticky bit** on a directory ("only the owner may delete or rename",
  shown in File Station as *Only the owner can delete files and folders*, and as
  `1777`/`3777` in `ls -ld`). It blocks other accounts from renaming files even
  in a world-writable folder.
- **Windows ACL support / Advanced Folder Permissions** (Control Panel →
  Privilege → Shared Folders → Advanced Permissions). With Windows ACLs on, an
  NT ACL in the folder's extended attributes is authoritative and the POSIX bits
  become cosmetic — `getfacl` will show extra entries. Keep both off for a
  POSIX/group-based setup, and note QNAP warns that toggling them can drop
  existing permissions.

After this, both identities can fully manage every file, no more `chown` over
SSH. Existing files need the one-time `chmod`/`chgrp` above; everything created
afterwards inherits it automatically.

> Which PUID should audioborker use? Whichever account already owns your media —
> i.e. the same service account your other *arr apps run as. It is the group
> and umask, not the uid, that let your personal account co-manage the results.
> If one group can't cover everything, `SUPPLEMENTARY_GIDS=100,1000` adds extra
> group memberships to the container's user.

> **Caveat:** if the share has QNAP's *Advanced Folder Permissions* or Windows
> ACLs enabled, those can override POSIX permissions and silently undo the
> above. Check Control Panel → Privilege → Shared Folders → Edit Permissions
> before debugging further.

## 3. Create the application

Container Station → **Applications** → **Create** → give it the name
`audioborker`, then paste the YAML from
[`docker-compose.qnap.yml`](../docker-compose.qnap.yml), editing:

- the four host paths (`/share/...`) to match your actual shares,
- `PUID` / `PGID` from step 2,
- `TZ` to your timezone.

Notes on the paths:

| Container path | What it is | Requirements |
|---|---|---|
| `/config` | SQLite database + job logs | **Local disk only.** SQLite's WAL journal breaks on NFS/SMB mounts. `/share/Container/...` is fine. |
| `/input` | Where raw audiobooks arrive | Read access; write access too if you use the move/delete cleanup modes. |
| `/output` | Your audiobook library | Write access. Point it at the folder Plex/Prologue already scans. |
| `/completed` | Optional | Only used by the "move" cleanup mode. Must be **outside** `/input` — the app enforces this. |

Click **Create**. Container Station pulls the image and starts it.

## 4. First run

Open `http://<nas>:8684` and go to **Settings** first. The defaults already
match the volume layout above, but confirm:

- **Output path template** — default
  `{author}/{series_name}/{Book series_position:00 - }{title}/{title} [{asin}]`; the preview shows
  both a series book and a standalone as you type.
- **Region** — `us` unless your ASINs come from another Audible storefront.
- **Cleanup mode** — starts at "leave sources in place"; switch to move/delete
  once you trust it.

Then **Import → Match → Queue**. Watch the first conversion's log on the job
detail page; a working run ends with a `verified: …, N chapters` line.

## 5. Updating

Container Station → Applications → `audioborker` → **Recreate** (or *Pull* then
restart) fetches the newest `:latest`. Your database and settings live in
`/config`, so they survive updates.

To pin a specific version instead of tracking `latest`, tag a release
(`git tag v1.0.0 && git push --tags`) and use
`ghcr.io/lbinderup/audioborker:1.0.0` in the compose file.

## Troubleshooting

**Permission denied / read-only errors in a job log** — `PUID`/`PGID` don't
match your share ownership. Recheck step 2. The container deliberately never
chowns `/input` or `/output`; it adapts to your ids instead of rewriting your
library's permissions.

**Converted files land where my personal account can't delete them** — the
container is writing 644 files. Confirm `UMASK=002` is set on the container and
that the output folder has the shared group plus its setgid bit (step 2b).
Verify what actually got written with `ls -l` — you want `-rw-rw-r--` and a
group both accounts belong to.

**"database is locked" or corruption on startup** — `/config` is on a network
mount. Move it to local storage.

**Progress bars don't update behind a reverse proxy** — the UI streams updates
over Server-Sent Events. Add `proxy_buffering off;` for `/events` in nginx.
Direct access on port 8684 is unaffected.

**A book fails with "no supported audio files"** — the folder contains formats
ffmpeg can't decode, or only non-audio files. Check the job log for what it saw.

**Container won't pull** — the GHCR package is still private; make it public or
add the registry credentials (step 1).

## A note on licensing

The published image bundles FFmpeg, which Alpine builds under GPL-3.0-or-later.
Distributing the image means distributing FFmpeg, so if you share it beyond
your own NAS, include the license text and a source offer. Running it privately
carries no such obligation. See [THIRD-PARTY-NOTICES.md](../THIRD-PARTY-NOTICES.md).
