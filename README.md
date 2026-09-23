# gdrive-ignore

Exclude files and folders from **Google Drive for Desktop** sync with `.gitignore`-style rules.

Drive for Desktop has no exclude setting. gdrive-ignore keeps a **filtered mirror** of each folder you choose inside a location that Drive syncs, leaving out everything your rules ignore. Drive then uploads the mirror as usual.

- Single self-contained `.exe` (~12 MB), no admin rights, no runtime to install
- No Google account access or credentials; it only works with local folders
- Changes are mirrored within about a second (recursive file watching), with a periodic full rescan as a safety net
- No extra disk space when the mirror is on the same drive as the source (NTFS hardlinks)
- Windows 10/11 for now; the core is portable, so macOS can follow

## How it works

```
C:\code\my-app            (your folder, only ever read)
  src\                    ──►  C:\Users\you\DriveMirror\my-app\src\     ──►  Google Drive
  node_modules\   ✗ ignored
  build\          ✗ ignored
  .driveignore
```

You pick a **source** (the folder you work in) and a **target** (a folder Drive syncs). The app mirrors the source into the target and skips ignored paths:

| Target | Disk space | Where it shows up in Drive |
|---|---|---|
| A local folder such as `C:\Users\you\DriveMirror`, added once in Drive under *Settings → Preferences → My Computer → Add folder* | None extra: files are hardlinked | *Computers* section |
| `G:\My Drive\…` or a shared drive (Drive's virtual drive in streaming mode) | Files are copied | *My Drive* / the shared drive |
| A subfolder of a folder already synced in mirror mode | None extra if on the same drive | wherever that folder syncs |

The app detects Drive's locations automatically from its local settings and suggests the best option.

## Rules

Rules use `.gitignore` syntax, one pattern per line:

```gitignore
node_modules/      # a folder, anywhere
*.log              # files by pattern, anywhere
/build             # only at the top of the source folder
docs/**/*.pdf      # ** matches any number of folders
!keep.log          # re-include something an earlier rule excluded
# comment
```

Rules come from three places. Later ones take precedence:

1. **Global rules**: apply to every folder (editable in the app, or in `%APPDATA%\gdrive-ignore\global.driveignore`).
2. **Folder rules**: typed in the app or imported from any ignore file.
3. **`.driveignore` files** inside the source folder, applied to their own folder and below, like nested `.gitignore` files. Optionally, `.gitignore` files are honored too.

Matching is case-insensitive, as on Windows file systems. Backslashes work as path separators.

## Install

1. Download `gdrive-ignore.exe` and run it.
2. Click **Install for this user**. This copies it to `%LOCALAPPDATA%\Programs\gdrive-ignore`, adds a Start menu entry, starts it with Windows, and registers an uninstaller in *Apps & features*.
3. Click **Add folder**, choose the source, the target and your rules, then **Preview** to see exactly what will be left out.

It runs from the tray. Close the window any time; syncing continues in the background.

## Command line

The same exe works as a CLI:

```
gdrive-ignore status                 show folders and their state
gdrive-ignore sync                   rescan all folders now
gdrive-ignore check <path>           explain whether a path is ignored, and by which rule
gdrive-ignore preview <folder> [--rules file] [--no-global] [--gitignore]
gdrive-ignore roots                  list the folders Google Drive syncs
gdrive-ignore install | uninstall [--purge]
```

## Safety

- The source folder is never written to. Deleting from the mirror uses a method that doesn't clear read-only flags, because with hardlinks that would change the source too.
- The app only deletes files it created. It keeps a manifest of them, so files that were already in a target folder are left alone. Using a non-empty target requires confirmation.
- Folders that overlap (target inside source, two pairs sharing a target) are rejected.
- The mirror is **one-way**. Edit files in the source. Changes made in the mirror (for example from another computer through Drive) are overwritten on the next sync.
- Deleted mirror files go to Drive's trash, so they stay recoverable for 30 days.

## Build

Requires Go 1.27+.

```powershell
.\scripts\build.ps1 -Version 0.1.0     # -> dist\gdrive-ignore.exe
go test ./...
```

Release builds should be **code-signed**. Windows Smart App Control can block unsigned executables based on a per-file reputation check.

## Layout

```
cmd/gdrive-ignore   entry point: tray agent, window, CLI
internal/ignore     gitignore-style matcher and nested rule loading
internal/mirror     filtered mirror engine (hardlink/copy, manifest, safety)
internal/watch      recursive ReadDirectoryChangesW watcher
internal/runner     per-folder sync loop (debounce, coalescing, rescans)
internal/drivefs    Drive for Desktop location discovery (registry + settings DB)
internal/agent      configuration, running pairs, local HTTP API
internal/web        embedded UI (HTML/CSS/JS, no build step)
internal/ui, tray   WebView2 window and tray icon
internal/install    per-user install, autostart, uninstall
```
