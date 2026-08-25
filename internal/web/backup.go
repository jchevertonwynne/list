package web

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// handleBackup streams a consistent snapshot of the database, for the hourly
// backup CronJob in jchevertonwynne/homelab to commit to homelab-backups.
//
// This exists as an endpoint rather than as something the backup job does for
// itself, because none of the alternatives work. The image is FROM scratch, so
// there is no shell to run and no sqlite3 to run .backup with, and kubectl exec
// has nothing to exec. Copying the files off the PersistentVolumeClaim from the
// node looks like it would work and does not: WAL mode means recent commits sit
// in list.db-wal, which on the Pi is routinely larger than list.db itself, so a
// copy of the database alone quietly loses data. Copying all three files
// together is a torn read unless the app is stopped first, which turns an
// hourly backup into hourly downtime. Only the running process can take a
// consistent snapshot without stopping, which makes it the running process's
// job to offer one.
//
// Two properties of this handler are deliberate and worth keeping:
//
// It is unauthenticated at the application level, like /metrics, because the
// caller is in-cluster and has no Cloudflare Access session to present. That is
// a bigger claim for a database dump than for a metrics scrape, so state the
// boundary plainly: what protects this is Cloudflare Access on
// list.jchevertonwynne.uk and nothing else. Anyone the Access policy admits can
// fetch every collection, every item and every cover in one request. That is
// accepted for a household list whose allowlist is a handful of addresses in
// infrastructure/cloudflared/access-policy.yaml, and it is the reason that file
// is the thing to be careful with. It would not survive that allowlist growing
// into something less personal.
//
// It does real work — a full VACUUM INTO, proportional to the size of the
// database — which /healthz and /metrics deliberately do not. Nothing here
// rate-limits it. That is tolerable only because of the sentence above: the set
// of callers who can reach it is the set of people already trusted with the
// data. It is not a public endpoint that happens to be expensive.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	// os.MkdirTemp("") honours TMPDIR and falls back to /tmp. The container
	// runs with readOnlyRootFilesystem, so this only works because
	// apps/list/deployment.yaml mounts a tmp emptyDir there — the same pairing
	// weight-tracker documents, where the endpoint returned 500 from the day it
	// shipped until the day someone first called it. A snapshot in the PVC
	// beside the live database was the alternative; a directory this handler
	// owns and removes cannot be mistaken for data.
	dir, err := os.MkdirTemp("", "list-backup")
	if err != nil {
		internalError(w, "handleBackup: MkdirTemp", err)
		return
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			// Not surfaced to the caller: the backup itself has already
			// succeeded by this point, and a leaked temporary directory is a
			// disk-space problem for the next call to notice, not a reason to
			// tell the CronJob its snapshot failed.
			log.Printf("handleBackup: RemoveAll(%s): %v", dir, err)
		}
	}()

	// Must not already exist — VACUUM INTO refuses to overwrite, which is why
	// this is a fresh directory rather than a fixed path.
	path := filepath.Join(dir, "list.db")
	if err := s.store.Backup(r.Context(), path); err != nil {
		internalError(w, "handleBackup: Backup", err)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		internalError(w, "handleBackup: Open", err)
		return
	}
	defer f.Close()

	// Content-Length from the snapshot on disk, so a truncated transfer is
	// detectable by the caller rather than arriving as a short but
	// syntactically valid file. Nothing is written to w before this point:
	// every failure above still has the chance to be an honest 500.
	info, err := f.Stat()
	if err != nil {
		internalError(w, "handleBackup: Stat", err)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, f); err != nil {
		// Too late for a status code — the header is already out — so this is
		// logged rather than reported. The Content-Length above is what makes
		// the caller notice.
		log.Printf("handleBackup: streaming %s: %v", path, err)
	}
}
