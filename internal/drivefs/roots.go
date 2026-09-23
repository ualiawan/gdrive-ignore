package drivefs

import (
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// readRoots lists folders Drive syncs from root_preference_sqlite.db. The
// database (and its WAL) is copied first so Drive's files are never locked.
func readRoots(dir string) ([]Location, error) {
	src := filepath.Join(dir, "root_preference_sqlite.db")
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	tmp, err := os.MkdirTemp("", "gdrive-ignore-roots")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "roots.db")
	for _, suffix := range []string{"", "-wal"} {
		if err := copyShared(src+suffix, dst+suffix); err != nil && !(suffix != "" && errors.Is(err, fs.ErrNotExist)) {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", dst)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT r.title, r.root_path, r.is_my_drive, r.account_token, COALESCE(m.last_mount_point, '')
		FROM roots r LEFT JOIN media m ON m.media_id = r.media_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		var title, root, account, mount string
		var myDrive bool
		if err := rows.Scan(&title, &root, &myDrive, &account, &mount); err != nil {
			return out, err
		}
		p := root
		if !filepath.IsAbs(p) && mount != "" {
			p = filepath.Join(mount, root)
		}
		l := Location{Kind: KindBackup, Path: p, Label: title, Account: account}
		if myDrive {
			l.Kind = KindMirror
			l.Label = "My Drive (mirrored)"
		}
		if l.Label == "" {
			l.Label = filepath.Base(p)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func copyShared(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
