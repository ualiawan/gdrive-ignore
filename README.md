# gdrive-ignore

Two-way sync between a folder on your PC and **Google Drive for Desktop**, with `.gitignore`-style rules to leave things out.

Drive for Desktop has no exclude setting. gdrive-ignore keeps your folder in two-way sync with a **hardlinked Drive copy** inside a folder that Drive syncs, and leaves out everything your rules ignore. It behaves like normal Google Drive between your folder and Drive: changes and deletions flow both ways.

- Single self-contained `.exe` (~14 MB), no admin rights, no runtime to install
- No Google account access or credentials; it only works with local folders
- The Drive copy takes **no extra disk space** (NTFS hardlinks)
- Changes sync within about a second, with a periodic full rescan as a safety net
- Windows 10/11 for now

## How it works

```
C:\code\my-app  (your folder)  ⇄  C:\Users\you\DriveMirror\my-app  (Drive copy)  ⇄  Google Drive  ⇄  other computers
  src\           synced
  node_modules\  ignored: never crosses in either direction
```

You add `C:\Users\you\DriveMirror` to Drive once (*Settings → Preferences → My Computer → Add folder*, "Sync with Google Drive"). Online, your folders appear under **Computers**.

| What happens | Result |
|---|---|
| Add, edit or rename in your folder | Reaches Drive and your other computers |
| Add, edit or rename online or on another computer | Reaches your folder |
| Delete in your folder | Deleted in Drive (Drive keeps it in its trash for 30 days) |
| Delete online or on another computer | Moved to the **Windows Recycle Bin** on this PC, never deleted permanently |
| Delete by hand **in the Drive copy** | Never reaches your folder: gdrive-ignore puts it back |
| Edited in two places between syncs | Both kept: the older one is saved as `name (conflict <date>).ext` |
| Ignored files | Never sync in either direction; adding a rule never deletes anything |

### How it tells deletions apart

When a file disappears from the Drive copy, gdrive-ignore checks Drive for Desktop's own log and database (read-only):

- **Deleted in Drive first:** Drive applied a cloud change just before the file disappeared, pushed nothing up, and shows the item as trashed. The deletion is applied to your folder (to the Recycle Bin).
- **Deleted by hand in the Drive copy:** the file disappeared first, and Drive then uploaded the deletion. The file is restored from your folder.
- **Anything else** (for example gdrive-ignore wasn't running, or Drive changed its log format): nothing is deleted. The window lists the file with **Delete from this PC** and **Put back in Drive**.

More than 100 files deleted in Drive at once also wait for confirmation.

## Rules

`.gitignore` syntax, one pattern per line:

```gitignore
node_modules/      # a folder, anywhere
*.log              # files by pattern, anywhere
/build             # only at the top of your folder
docs/**/*.pdf      # ** matches any number of folders
!keep.log          # re-include something an earlier rule excluded
```

Rules come from global rules (all folders), per-folder rules (typed or imported from any ignore file), and `.driveignore` files inside your folder (nested, like `.gitignore`). Optionally, `.gitignore` files are honored too. Matching is case-insensitive.

## Install

1. Run `gdrive-ignore.exe` and click **Install for this user**. It installs to `%LOCALAPPDATA%\Programs\gdrive-ignore`, adds a Start menu entry, starts with Windows, adds the command line to your PATH, and registers an uninstaller.
2. In Google Drive, add `C:\Users\<you>\DriveMirror` under *My Computer* once.
3. Click **Add folder**, pick your folder, set rules, click **Preview**, then **Save and start syncing**.

## Command line

```
gdrive-ignore status              folders, state and deletions waiting for a decision
gdrive-ignore sync                rescan all folders now
gdrive-ignore check <path>        explain whether a path is ignored, and by which rule
gdrive-ignore preview <folder> [--rules file] [--no-global] [--gitignore]
gdrive-ignore roots               list the folders Google Drive syncs
gdrive-ignore install | uninstall [--purge]
```

## Safety

- Nothing is ever deleted permanently from your folder: deletions coming from Drive go to the Recycle Bin.
- A deletion by hand in the Drive copy can never delete a file from your folder.
- Anything uncertain waits for you; large deletions from Drive wait for confirmation.
- Stopping sync for a folder never deletes files on either side.
- Your folder is changed only to apply changes from Drive: renames, new files and new versions (applied atomically), and Recycle Bin moves.
- Folders that overlap are rejected. The Drive copy must be on the same drive as your folder.

## Build

Requires Go 1.27+.

```powershell
.\scripts\build.ps1 -Version 0.4.0     # -> dist\gdrive-ignore.exe
go test ./...
```

Release builds should be **code-signed**: Windows Smart App Control can block unsigned executables.

## Layout

```
cmd/gdrive-ignore      entry point: tray agent, window, CLI
cmd/gdrive-ignore-cli  console launcher (installed as gdrive-ignore.com)
internal/ignore        gitignore-style matcher and nested rule loading
internal/twoway        two-way sync engine (baseline, renames, conflicts, deletions)
internal/origin        decides where a Drive-copy deletion started
internal/drivelog      follows Drive for Desktop's log
internal/drivefs       Drive location discovery and read-only item lookups
internal/watch         recursive ReadDirectoryChangesW watcher
internal/runner        per-folder sync loop (both folders watched)
internal/agent         configuration, running folders, local HTTP API
internal/web           embedded UI (HTML/CSS/JS, no build step)
internal/ui, tray      WebView2 window and tray icon
internal/install       per-user install, autostart, PATH, uninstall
```
