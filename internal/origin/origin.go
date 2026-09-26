// Package origin decides where a deletion in the mirror folder started:
// in Google Drive (online or on another computer), or by hand in the mirror.
//
// Rule (verified against a real Drive for Desktop, see PLAN.md):
//   - Drive-first: Drive logs "download events" (applying a cloud change)
//     just before the local file disappears, logs no "upload events" right
//     after, and its database shows the item trashed. → apply to the source.
//   - Mirror-first: the local file disappears first and Drive logs "upload
//     events" about a second later to push the deletion. → restore it.
//   - Anything else is uncertain and left for the user.
//
// Every failure (log not readable, format changed, no timestamp, database
// unreadable) falls through to uncertain, never to deleting.
package origin

import (
	"time"

	"gdrive-ignore/internal/drivelog"
	"gdrive-ignore/internal/twoway"
)

// Timing windows around the moment a mirror entry was seen disappearing.
const (
	downloadBefore = 3 * time.Second        // a cloud change applied just before
	downloadAfter  = 500 * time.Millisecond // (log/watch timing slack)
	uploadBefore   = 500 * time.Millisecond
	uploadAfter    = 6 * time.Second // Drive pushes a local delete within ~1 s
	settle         = uploadAfter + 500*time.Millisecond
)

// Log is what the checker needs from the Drive log tail.
type Log interface {
	Poll()
	Covers(t time.Time) bool
	Any(kind drivelog.Kind, account string, from, to time.Time) bool
}

// Checker implements twoway.Classifier for one Drive account.
type Checker struct {
	Log           Log
	Account       string
	RemoteDeleted func(ids []int64) (bool, error)
	Now           func() time.Time
}

// Classify applies the rule above.
func (c *Checker) Classify(d twoway.Deletion) twoway.Origin {
	if c == nil || c.Log == nil || c.Account == "" || d.Gone.IsZero() {
		return twoway.OriginUnknown
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if now().Before(d.Gone.Add(settle)) {
		return twoway.OriginWait
	}
	c.Log.Poll()
	if !c.Log.Covers(d.Gone.Add(-downloadBefore)) {
		return twoway.OriginUnknown
	}
	dl := c.Log.Any(drivelog.Download, c.Account, d.Gone.Add(-downloadBefore), d.Gone.Add(downloadAfter))
	ul := c.Log.Any(drivelog.Upload, c.Account, d.Gone.Add(-uploadBefore), d.Gone.Add(uploadAfter))
	switch {
	case ul && !dl:
		return twoway.OriginMirror
	case dl && !ul:
		if c.RemoteDeleted == nil {
			return twoway.OriginUnknown
		}
		deleted, err := c.RemoteDeleted(d.DriveIDs)
		if err == nil && deleted {
			return twoway.OriginDrive
		}
	}
	return twoway.OriginUnknown
}
