// Package store persists the domain types in types.go to a SQLite file.
//
// The driver is modernc.org/sqlite: pure Go, so CGO_ENABLED=0 builds still
// link it, and the production image (FROM scratch, no libc) has no other
// option. The cost is a slower query path than mattn's cgo binding; for a
// household to-do list on a Pi that trade-off is not close.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB is a SQLite-backed store. The zero value is not usable; construct one
// with Open.
type DB struct {
	db *sql.DB
}

// migrations is applied in order, gated on PRAGMA user_version so that
// re-running Open against an already-migrated file is a no-op rather than a
// pile of "table already exists" errors. Tables are ordered so that every
// REFERENCES clause points at something already created; SQLite does not
// check this at CREATE TABLE time, but a foreign key to a table that never
// arrives is a bug worth avoiding rather than relying on the omission.
var migrations = []string{
	`CREATE TABLE users (
		id         INTEGER PRIMARY KEY,
		email      TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE collections (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		owner_id   INTEGER NOT NULL REFERENCES users(id),
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE memberships (
		collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
		user_id       INTEGER NOT NULL REFERENCES users(id),
		created_at    INTEGER NOT NULL,
		PRIMARY KEY (collection_id, user_id)
	)`,
	`CREATE TABLE items (
		id            INTEGER PRIMARY KEY,
		collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
		title         TEXT NOT NULL,
		body          TEXT NOT NULL DEFAULT '',
		done          INTEGER NOT NULL DEFAULT 0,
		creator_id    INTEGER NOT NULL REFERENCES users(id),
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,
	// Every membership lookup in CollectionsForUser goes in by user_id, and
	// every cascade delete of a collection goes in by collection_id (the
	// primary key covers that side already); this index is for the former.
	`CREATE INDEX idx_memberships_user ON memberships (user_id)`,
	// Items is a fan-out table an order of magnitude bigger than the others;
	// Items and CollectionsForUser both filter on collection_id.
	`CREATE INDEX idx_items_collection ON items (collection_id)`,
	// Manual ordering: undone items in drag order, then done items in drag
	// order. See the ORDER BY in Items for the full three-term sort this
	// column participates in.
	`ALTER TABLE items ADD COLUMN position INTEGER NOT NULL DEFAULT 0`,
	// Every row added before this migration would otherwise share position 0
	// and fall back entirely to the id tiebreak in the ORDER BY below, which
	// happens to reproduce today's order only by luck (ids already increase
	// in creation order). Seeding position from id makes that existing order
	// explicit and stable instead of an accident of the tiebreak.
	`UPDATE items SET position = id`,
}

// Open opens (creating if necessary) the SQLite file at path and brings its
// schema up to date.
//
// The DSN pragmas are the whole ballgame:
//
//   - foreign_keys(1): database/sql pools connections, and each one gets this
//     DSN independently. Setting the pragma once on a single *sql.Conn after
//     Open is not enough — the next checked-out connection from the pool
//     silently has foreign keys off again, and ON DELETE CASCADE stops
//     firing with no error at all. It has to live in the DSN so every
//     connection the pool ever opens gets it.
//   - journal_mode(WAL): readers do not block the writer, which matters once
//     more than one handler goroutine hits the database at once.
//   - busy_timeout(5000): under WAL a second writer still has to wait for
//     the first; this makes it retry for 5s instead of failing immediately
//     with SQLITE_BUSY.
//   - temp_store(MEMORY): the container is FROM scratch with a read-only
//     root filesystem, so there is no /tmp for SQLite to spill sort or
//     temp-table data into. Keeping it in memory is not an optimisation
//     here, it is the only option that does not crash.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)",
		path,
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	d := &DB{db: sqlDB}
	if err := d.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the underlying connection pool.
func (d *DB) Close() error {
	return d.db.Close()
}

// migrate applies whichever migrations are newer than the schema already on
// disk. user_version lives in the SQLite file header, not in a connection, so
// it is a reliable gate even though the pool may hand out a fresh connection
// for this call and a different one for the next.
func (d *DB) migrate(ctx context.Context) error {
	var version int
	if err := d.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= len(migrations) {
		return nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range migrations[version:] {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration: %w", err)
		}
	}

	// PRAGMA does not accept a bound parameter, so the version is
	// interpolated directly; it is an int we produced ourselves (len of a
	// package-level slice), never external input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}

	return tx.Commit()
}

// normaliseEmail collapses case and incidental whitespace so that
// "Alice@x.com" and " alice@x.com " address the same user row. Cloudflare
// Access headers and manually typed invitations do not agree on case, and
// without this every mismatch would silently create a second, unreachable
// account.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// notFoundIfNoRows turns a zero-rows-affected Exec result into ErrNotFound.
// Every mutation below (rename, delete, toggle) targets a row by id with no
// prior existence check, so this is the one place that distinction is made.
func notFoundIfNoRows(res sql.Result, format string, args ...any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		args = append(args, ErrNotFound)
		return fmt.Errorf(format+": %w", args...)
	}
	return nil
}

// UserByEmail upserts: an email that has never been seen becomes a user.
// Both authentication (Access hands us an email on every request) and
// invitation (naming someone before they have ever logged in) depend on this
// never returning ErrNotFound.
func (d *DB) UserByEmail(ctx context.Context, email string) (User, error) {
	return withSpan(ctx, "UserByEmail", func(ctx context.Context) (User, error) {
		email = normaliseEmail(email)
		now := time.Now().Unix()

		if _, err := d.db.ExecContext(ctx, `
			INSERT INTO users (email, created_at) VALUES (?, ?)
			ON CONFLICT (email) DO NOTHING
		`, email, now); err != nil {
			return User{}, fmt.Errorf("upsert user %s: %w", email, err)
		}

		var u User
		var createdAt int64
		err := d.db.QueryRowContext(ctx, `SELECT id, email, created_at FROM users WHERE email = ?`, email).
			Scan(&u.ID, &u.Email, &createdAt)
		if err != nil {
			// Not mapped to ErrNotFound: the row was just inserted or already
			// existed, so its absence here means something else went wrong.
			return User{}, fmt.Errorf("load user %s: %w", email, err)
		}
		u.CreatedAt = time.Unix(createdAt, 0).UTC()
		return u, nil
	})
}

// CollectionsForUser returns collections the user is a member of, which
// includes ones they own since CreateCollection gives the owner a membership
// row too. ItemCount and DoneCount are aggregated here rather than per-row in
// the handler so the index page is one query regardless of how many
// collections or items exist.
func (d *DB) CollectionsForUser(ctx context.Context, userID int64) ([]CollectionView, error) {
	return withSpan(ctx, "CollectionsForUser", func(ctx context.Context) ([]CollectionView, error) {
		rows, err := d.db.QueryContext(ctx, `
			SELECT c.id, c.name, c.owner_id, c.created_at, o.email,
			       COUNT(i.id) AS item_count,
			       COALESCE(SUM(i.done), 0) AS done_count
			FROM memberships m
			JOIN collections c ON c.id = m.collection_id
			JOIN users o ON o.id = c.owner_id
			LEFT JOIN items i ON i.collection_id = c.id
			WHERE m.user_id = ?
			GROUP BY c.id
			ORDER BY c.created_at DESC
		`, userID)
		if err != nil {
			return nil, fmt.Errorf("list collections for user %d: %w", userID, err)
		}
		defer rows.Close()

		var views []CollectionView
		for rows.Next() {
			var v CollectionView
			var createdAt int64
			if err := rows.Scan(&v.ID, &v.Name, &v.OwnerID, &createdAt, &v.OwnerEmail, &v.ItemCount, &v.DoneCount); err != nil {
				return nil, fmt.Errorf("scan collection: %w", err)
			}
			v.CreatedAt = time.Unix(createdAt, 0).UTC()
			views = append(views, v)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list collections for user %d: %w", userID, err)
		}
		return views, nil
	})
}

// CreateCollection inserts the collection and the owner's membership row in
// one transaction. Without the membership row in the same transaction, a
// crash or a concurrent read between the two inserts would show the owner a
// collection list that does not include the collection they just made.
func (d *DB) CreateCollection(ctx context.Context, ownerID int64, name string) (Collection, error) {
	return withSpan(ctx, "CreateCollection", func(ctx context.Context) (Collection, error) {
		now := time.Now().Unix()

		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return Collection{}, fmt.Errorf("begin create collection: %w", err)
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(ctx, `INSERT INTO collections (name, owner_id, created_at) VALUES (?, ?, ?)`, name, ownerID, now)
		if err != nil {
			return Collection{}, fmt.Errorf("insert collection: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Collection{}, fmt.Errorf("collection id: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (collection_id, user_id, created_at) VALUES (?, ?, ?)`, id, ownerID, now); err != nil {
			return Collection{}, fmt.Errorf("insert owner membership: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return Collection{}, fmt.Errorf("commit create collection: %w", err)
		}

		return Collection{ID: id, Name: name, OwnerID: ownerID, CreatedAt: time.Unix(now, 0).UTC()}, nil
	})
}

// Collection loads a single collection by id.
func (d *DB) Collection(ctx context.Context, id int64) (Collection, error) {
	return withSpan(ctx, "Collection", func(ctx context.Context) (Collection, error) {
		var c Collection
		var createdAt int64
		err := d.db.QueryRowContext(ctx, `SELECT id, name, owner_id, created_at FROM collections WHERE id = ?`, id).
			Scan(&c.ID, &c.Name, &c.OwnerID, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return Collection{}, fmt.Errorf("collection %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return Collection{}, fmt.Errorf("load collection %d: %w", id, err)
		}
		c.CreatedAt = time.Unix(createdAt, 0).UTC()
		return c, nil
	})
}

// IsMember reports whether userID may see collectionID. A row's absence is
// not an error here — the caller uses this to decide whether to return 404,
// so returning ErrNotFound from inside the check it is deciding would be
// circular.
func (d *DB) IsMember(ctx context.Context, collectionID, userID int64) (bool, error) {
	return withSpan(ctx, "IsMember", func(ctx context.Context) (bool, error) {
		var exists int
		err := d.db.QueryRowContext(ctx, `SELECT 1 FROM memberships WHERE collection_id = ? AND user_id = ?`, collectionID, userID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check membership of %d in collection %d: %w", userID, collectionID, err)
		}
		return true, nil
	})
}

// RenameCollection updates the display name.
func (d *DB) RenameCollection(ctx context.Context, id int64, name string) error {
	return withSpanErr(ctx, "RenameCollection", func(ctx context.Context) error {
		res, err := d.db.ExecContext(ctx, `UPDATE collections SET name = ? WHERE id = ?`, name, id)
		if err != nil {
			return fmt.Errorf("rename collection %d: %w", id, err)
		}
		return notFoundIfNoRows(res, "collection %d", id)
	})
}

// DeleteCollection removes the collection. Its memberships and items go with
// it via ON DELETE CASCADE — see the foreign_keys(1) pragma in Open for why
// that cascade is trustworthy at all.
func (d *DB) DeleteCollection(ctx context.Context, id int64) error {
	return withSpanErr(ctx, "DeleteCollection", func(ctx context.Context) error {
		res, err := d.db.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete collection %d: %w", id, err)
		}
		return notFoundIfNoRows(res, "collection %d", id)
	})
}

// Items lists a collection's items with the creator's email resolved by
// join, so rendering a list of N items costs one query rather than N+1.
//
// ORDER BY done ASC, position ASC, id ASC: undone items first in their manual
// drag order, then done items in their manual drag order. Ticking an item
// sinks it below every undone item purely as a consequence of this sort —
// see ToggleItem, which deliberately never touches position — and unticking
// it surfaces it back where it was among the undone items, again with no
// write. id is the final tiebreak for rows that still share a position (e.g.
// two items both backfilled to the same value, which cannot happen after the
// migration but is cheap insurance against it happening some other way).
func (d *DB) Items(ctx context.Context, collectionID int64) ([]ItemView, error) {
	return withSpan(ctx, "Items", func(ctx context.Context) ([]ItemView, error) {
		rows, err := d.db.QueryContext(ctx, `
			SELECT i.id, i.collection_id, i.title, i.body, i.done, i.creator_id, i.created_at, i.updated_at, u.email
			FROM items i
			JOIN users u ON u.id = i.creator_id
			WHERE i.collection_id = ?
			ORDER BY i.done ASC, i.position ASC, i.id ASC
		`, collectionID)
		if err != nil {
			return nil, fmt.Errorf("list items for collection %d: %w", collectionID, err)
		}
		defer rows.Close()

		var views []ItemView
		for rows.Next() {
			var v ItemView
			var createdAt, updatedAt int64
			if err := rows.Scan(&v.ID, &v.CollectionID, &v.Title, &v.Body, &v.Done, &v.CreatorID, &createdAt, &updatedAt, &v.CreatorEmail); err != nil {
				return nil, fmt.Errorf("scan item: %w", err)
			}
			v.CreatedAt = time.Unix(createdAt, 0).UTC()
			v.UpdatedAt = time.Unix(updatedAt, 0).UTC()
			views = append(views, v)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list items for collection %d: %w", collectionID, err)
		}
		return views, nil
	})
}

// CreateItem inserts a new item and re-reads it through Item so the caller
// gets the creator's email back without duplicating that join here.
//
// position is one past the current maximum within the collection, which
// lands the new item at the end of the undone group — the subquery scopes
// MAX(position) to collection_id rather than the whole table, or a
// collection that started later than another would have its first item jump
// ahead of everything in the older one.
func (d *DB) CreateItem(ctx context.Context, collectionID, creatorID int64, title, body string) (ItemView, error) {
	return withSpan(ctx, "CreateItem", func(ctx context.Context) (ItemView, error) {
		now := time.Now().Unix()
		res, err := d.db.ExecContext(ctx, `
			INSERT INTO items (collection_id, title, body, creator_id, created_at, updated_at, position)
			VALUES (?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM items WHERE collection_id = ?))
		`, collectionID, title, body, creatorID, now, now, collectionID)
		if err != nil {
			return ItemView{}, fmt.Errorf("insert item: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return ItemView{}, fmt.Errorf("item id: %w", err)
		}
		return d.Item(ctx, id)
	})
}

// Item loads a single item with its creator's email resolved.
func (d *DB) Item(ctx context.Context, id int64) (ItemView, error) {
	return withSpan(ctx, "Item", func(ctx context.Context) (ItemView, error) {
		var v ItemView
		var createdAt, updatedAt int64
		err := d.db.QueryRowContext(ctx, `
			SELECT i.id, i.collection_id, i.title, i.body, i.done, i.creator_id, i.created_at, i.updated_at, u.email
			FROM items i
			JOIN users u ON u.id = i.creator_id
			WHERE i.id = ?
		`, id).Scan(&v.ID, &v.CollectionID, &v.Title, &v.Body, &v.Done, &v.CreatorID, &createdAt, &updatedAt, &v.CreatorEmail)
		if errors.Is(err, sql.ErrNoRows) {
			return ItemView{}, fmt.Errorf("item %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return ItemView{}, fmt.Errorf("load item %d: %w", id, err)
		}
		v.CreatedAt = time.Unix(createdAt, 0).UTC()
		v.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		return v, nil
	})
}

// UpdateItem changes title and body. Any member may call this — see the
// comment on Item.CreatorID in types.go — so there is no creator check here.
func (d *DB) UpdateItem(ctx context.Context, id int64, title, body string) (ItemView, error) {
	return withSpan(ctx, "UpdateItem", func(ctx context.Context) (ItemView, error) {
		res, err := d.db.ExecContext(ctx, `UPDATE items SET title = ?, body = ?, updated_at = ? WHERE id = ?`, title, body, time.Now().Unix(), id)
		if err != nil {
			return ItemView{}, fmt.Errorf("update item %d: %w", id, err)
		}
		if err := notFoundIfNoRows(res, "item %d", id); err != nil {
			return ItemView{}, err
		}
		return d.Item(ctx, id)
	})
}

// ToggleItem flips done. NOT done reads as a boolean flip in SQLite because
// the column is INTEGER NOT NULL DEFAULT 0, so it is always exactly 0 or 1
// going in.
//
// This deliberately never touches position. Items' ORDER BY (done ASC,
// position ASC, id ASC) already sinks a done item below every undone one and
// resurfaces it in its old spot the moment it is unticked again — rewriting
// position on every toggle would be redundant work that also throws away the
// place the item held among its own group, so an untick would land it
// somewhere new instead of back where it was.
func (d *DB) ToggleItem(ctx context.Context, id int64) (ItemView, error) {
	return withSpan(ctx, "ToggleItem", func(ctx context.Context) (ItemView, error) {
		res, err := d.db.ExecContext(ctx, `UPDATE items SET done = NOT done, updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
		if err != nil {
			return ItemView{}, fmt.Errorf("toggle item %d: %w", id, err)
		}
		if err := notFoundIfNoRows(res, "item %d", id); err != nil {
			return ItemView{}, err
		}
		return d.Item(ctx, id)
	})
}

// DeleteItem removes one item.
func (d *DB) DeleteItem(ctx context.Context, id int64) error {
	return withSpanErr(ctx, "DeleteItem", func(ctx context.Context) error {
		res, err := d.db.ExecContext(ctx, `DELETE FROM items WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete item %d: %w", id, err)
		}
		return notFoundIfNoRows(res, "item %d", id)
	})
}

// ReorderItems assigns positions to ids in the order given: the first id
// gets position 0, the second 1, and so on. All updates run in one
// transaction so a reorder is atomic from any reader's point of view — never
// half the drag applied.
//
// Every UPDATE is scoped WHERE id = ? AND collection_id = ?, and that
// scoping is the security boundary, not a redundant belt-and-braces check:
// an id smuggled in from another collection matches zero rows and is
// silently skipped rather than being adopted into this collection's order.
// A zero-row UPDATE is not surfaced as an error, unlike notFoundIfNoRows
// elsewhere in this file — an id that stopped existing mid-drag because of a
// concurrent delete is an ordinary race, not a failure worth aborting the
// whole reorder over, and the deleted item was never going to be seen out of
// order again anyway.
func (d *DB) ReorderItems(ctx context.Context, collectionID int64, ids []int64) error {
	return withSpanErr(ctx, "ReorderItems", func(ctx context.Context) error {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin reorder items in collection %d: %w", collectionID, err)
		}
		defer tx.Rollback()

		for position, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE items SET position = ? WHERE id = ? AND collection_id = ?
			`, position, id, collectionID); err != nil {
				return fmt.Errorf("reorder item %d in collection %d: %w", id, collectionID, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit reorder items in collection %d: %w", collectionID, err)
		}
		return nil
	})
}

// Members lists everyone who can see a collection, owner included.
func (d *DB) Members(ctx context.Context, collectionID int64) ([]User, error) {
	return withSpan(ctx, "Members", func(ctx context.Context) ([]User, error) {
		rows, err := d.db.QueryContext(ctx, `
			SELECT u.id, u.email, u.created_at
			FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE m.collection_id = ?
			ORDER BY u.email
		`, collectionID)
		if err != nil {
			return nil, fmt.Errorf("list members of collection %d: %w", collectionID, err)
		}
		defer rows.Close()

		var users []User
		for rows.Next() {
			var u User
			var createdAt int64
			if err := rows.Scan(&u.ID, &u.Email, &createdAt); err != nil {
				return nil, fmt.Errorf("scan member: %w", err)
			}
			u.CreatedAt = time.Unix(createdAt, 0).UTC()
			users = append(users, u)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list members of collection %d: %w", collectionID, err)
		}
		return users, nil
	})
}

// AddMember invites someone by email, creating the user row if this is the
// first time anyone has named them. Inviting the same person twice is not an
// error: ON CONFLICT DO NOTHING means the caller does not have to check
// membership first just to avoid a duplicate-key failure on a double click.
func (d *DB) AddMember(ctx context.Context, collectionID int64, email string) (User, error) {
	return withSpan(ctx, "AddMember", func(ctx context.Context) (User, error) {
		u, err := d.UserByEmail(ctx, email)
		if err != nil {
			return User{}, err
		}

		if _, err := d.db.ExecContext(ctx, `
			INSERT INTO memberships (collection_id, user_id, created_at) VALUES (?, ?, ?)
			ON CONFLICT (collection_id, user_id) DO NOTHING
		`, collectionID, u.ID, time.Now().Unix()); err != nil {
			return User{}, fmt.Errorf("add member %s to collection %d: %w", u.Email, collectionID, err)
		}
		return u, nil
	})
}

// RemoveMember revokes access. It does not touch the owner_id on the
// collection, so removing the owner from their own membership list (not
// exposed by any handler today) would leave a collection with an owner who
// cannot see it rather than deleting the collection — a deliberate gap
// left for the web layer to decide whether to allow.
func (d *DB) RemoveMember(ctx context.Context, collectionID, userID int64) error {
	return withSpanErr(ctx, "RemoveMember", func(ctx context.Context) error {
		res, err := d.db.ExecContext(ctx, `DELETE FROM memberships WHERE collection_id = ? AND user_id = ?`, collectionID, userID)
		if err != nil {
			return fmt.Errorf("remove member %d from collection %d: %w", userID, collectionID, err)
		}
		return notFoundIfNoRows(res, "member %d of collection %d", userID, collectionID)
	})
}
