package drivefs

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// These lookups read Drive's own databases for computer folders in place,
// strictly read-only (mode=ro, query_only) with short queries, so they never
// modify Drive's files and never hold Drive's writer up. SQLite's WAL mode
// lets readers and Drive's writer work concurrently.

func accountDir(account string) string { return filepath.Join(ConfigDir(), account) }

func openRO(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	u := "file:" + url.PathEscape(filepath.ToSlash(path)) + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", u)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// MirrorInodes maps local NTFS file IDs to Drive's item ids (stable ids) for
// the account's computer folders. Hardlinked source and mirror files share
// the file ID, so this ties every synced file to its Drive item.
func MirrorInodes(account string) (map[uint64]int64, error) {
	db, err := openRO(filepath.Join(accountDir(account), "mirror_sqlite.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT inode, stable_id FROM mirror_item`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]int64{}
	for rows.Next() {
		var inode, id int64
		if err := rows.Scan(&inode, &id); err != nil {
			return nil, err
		}
		out[uint64(inode)] = id
	}
	return out, rows.Err()
}

// ErrUnknownItem means none of the given items is known to Drive.
var ErrUnknownItem = errors.New("item not known to Google Drive")

// RemoteDeleted reports whether Drive's cloud state has any of the given
// items trashed or deleted. It returns ErrUnknownItem if Drive has no record
// of any of them.
func RemoteDeleted(account string, ids []int64) (bool, error) {
	if len(ids) == 0 {
		return false, ErrUnknownItem
	}
	db, err := openRO(filepath.Join(accountDir(account), "mirror_metadata_sqlite.db"))
	if err != nil {
		return false, err
	}
	defer db.Close()
	known := false
	for _, id := range ids {
		var trashed, tomb bool
		err := db.QueryRow(`SELECT trashed, is_tombstone FROM items WHERE stable_id = ?`, id).Scan(&trashed, &tomb)
		switch {
		case err == nil:
			known = true
			if trashed || tomb {
				return true, nil
			}
		case errors.Is(err, sql.ErrNoRows):
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM deleted_items WHERE stable_id = ?`, id).Scan(&n); err != nil {
				return false, err
			}
			if n > 0 {
				return true, nil
			}
		default:
			return false, err
		}
	}
	if !known {
		return false, ErrUnknownItem
	}
	return false, nil
}
