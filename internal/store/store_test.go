package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// open is the shared test helper: a temp file DB, not :memory:. An in-memory
// database is private to the connection that created it, and database/sql
// pools connections — a second query can land on a different, empty
// in-memory database with no error at all. A temp file behaves like
// production.
func open(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "list.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	// Re-opening the same file must not fail with "table already exists" —
	// that is the whole point of gating migrations on user_version.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	db2.Close()
}

func TestForeignKeysCascadeOnDelete(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := db.CreateItem(ctx, c.ID, owner.ID, "Milk", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	if err := db.DeleteCollection(ctx, c.ID); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	// If foreign_keys were silently off (the classic modernc/sqlite DSN
	// trap this pragma exists to avoid), the item row and the membership row
	// would both survive the delete instead of cascading away.
	if _, err := db.Item(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Item after cascade: got err %v, want ErrNotFound", err)
	}
	member, err := db.IsMember(ctx, c.ID, owner.ID)
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if member {
		t.Fatal("membership row survived collection delete; foreign_keys pragma is not taking effect")
	}
}

func TestUserByEmailUpsertAndNormalises(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	a, err := db.UserByEmail(ctx, "  Alice@Example.com ")
	if err != nil {
		t.Fatalf("UserByEmail (first): %v", err)
	}
	if a.Email != "alice@example.com" {
		t.Fatalf("email not normalised: got %q", a.Email)
	}

	b, err := db.UserByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("UserByEmail (second): %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("differently-cased email produced a second user: %d != %d", a.ID, b.ID)
	}

	// An email nobody has logged in with yet still becomes a user — this is
	// what lets AddMember invite someone who has never visited the site.
	unseen, err := db.UserByEmail(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("UserByEmail (unseen): %v", err)
	}
	if unseen.ID == a.ID {
		t.Fatal("unseen email collided with an existing user")
	}
}

func TestOwnerGetsMembershipAndAppearsInIndex(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	member, err := db.IsMember(ctx, c.ID, owner.ID)
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !member {
		t.Fatal("owner has no membership row after CreateCollection")
	}

	views, err := db.CollectionsForUser(ctx, owner.ID)
	if err != nil {
		t.Fatalf("CollectionsForUser: %v", err)
	}
	if len(views) != 1 || views[0].ID != c.ID {
		t.Fatalf("owner's own collection missing from CollectionsForUser: %+v", views)
	}
	if views[0].OwnerEmail != "owner@example.com" {
		t.Fatalf("OwnerEmail not resolved: %+v", views[0])
	}
}

func TestAddMemberTwiceIsNotAnError(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	if _, err := db.AddMember(ctx, c.ID, "friend@example.com"); err != nil {
		t.Fatalf("AddMember (first): %v", err)
	}
	if _, err := db.AddMember(ctx, c.ID, "friend@example.com"); err != nil {
		t.Fatalf("AddMember (second, duplicate invite): %v", err)
	}

	members, err := db.Members(ctx, c.ID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	// owner + friend, not owner + friend + friend.
	if len(members) != 2 {
		t.Fatalf("expected 2 members after double invite, got %d: %+v", len(members), members)
	}
}

func TestItemAndDoneCountsAggregate(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	i1, err := db.CreateItem(ctx, c.ID, owner.ID, "Milk", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := db.CreateItem(ctx, c.ID, owner.ID, "Eggs", ""); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := db.CreateItem(ctx, c.ID, owner.ID, "Bread", ""); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := db.ToggleItem(ctx, i1.ID); err != nil {
		t.Fatalf("ToggleItem: %v", err)
	}

	views, err := db.CollectionsForUser(ctx, owner.ID)
	if err != nil {
		t.Fatalf("CollectionsForUser: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(views))
	}
	if views[0].ItemCount != 3 {
		t.Fatalf("ItemCount = %d, want 3", views[0].ItemCount)
	}
	if views[0].DoneCount != 1 {
		t.Fatalf("DoneCount = %d, want 1", views[0].DoneCount)
	}

	items, err := db.Items(ctx, c.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("Items returned %d rows, want 3", len(items))
	}
	for _, it := range items {
		if it.CreatorEmail != "owner@example.com" {
			t.Fatalf("CreatorEmail not resolved via join: %+v", it)
		}
	}
}

// positionOf reads the position column directly, bypassing ItemView (which
// does not expose it — nothing outside this package needs to see a raw
// position, only the relative order it produces). Safe because store_test.go
// is part of package store and can reach the unexported *sql.DB underneath.
func positionOf(t *testing.T, db *DB, id int64) int64 {
	t.Helper()
	var pos int64
	if err := db.db.QueryRowContext(context.Background(), `SELECT position FROM items WHERE id = ?`, id).Scan(&pos); err != nil {
		t.Fatalf("query position of item %d: %v", id, err)
	}
	return pos
}

// TestBackfillSeedsPositionFromID simulates upgrading a database that
// predates the position column. Rows inserted before that migration must end
// up with position equal to their id, not all crammed onto the column
// default of 0 — sharing 0 would make every one of them depend on the id
// tiebreak in Items' ORDER BY by accident rather than by the explicit
// backfill this migration performs.
func TestBackfillSeedsPositionFromID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "list.db")

	// Open against only the migrations that predate the position column, so
	// the items table on disk has no position column yet.
	prePosition := len(migrations) - 2 // the ALTER TABLE and the backfill UPDATE
	original := migrations
	migrations = migrations[:prePosition]
	db, err := Open(path)
	migrations = original
	if err != nil {
		t.Fatalf("Open (pre-position schema): %v", err)
	}

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// Raw inserts rather than CreateItem: CreateItem's INSERT already
	// targets the position column added by the migration under test, so it
	// cannot be used to populate a database that predates it.
	now := time.Now().Unix()
	res1, err := db.db.ExecContext(ctx, `
		INSERT INTO items (collection_id, title, body, creator_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, c.ID, "Milk", "", owner.ID, now, now)
	if err != nil {
		t.Fatalf("insert pre-migration item: %v", err)
	}
	id1, _ := res1.LastInsertId()
	res2, err := db.db.ExecContext(ctx, `
		INSERT INTO items (collection_id, title, body, creator_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, c.ID, "Eggs", "", owner.ID, now, now)
	if err != nil {
		t.Fatalf("insert pre-migration item: %v", err)
	}
	id2, _ := res2.LastInsertId()
	db.Close()

	// Reopening with the full migration list runs the ALTER and the backfill
	// against these pre-existing rows.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (post-position schema): %v", err)
	}
	defer db2.Close()

	if got := positionOf(t, db2, id1); got != id1 {
		t.Fatalf("position of item %d = %d, want %d", id1, got, id1)
	}
	if got := positionOf(t, db2, id2); got != id2 {
		t.Fatalf("position of item %d = %d, want %d", id2, got, id2)
	}
}

// TestItemsOrdersUndoneBeforeDoneByPosition is the core property of the new
// sort: every undone item, in drag order, then every done item, in its own
// drag order — not one flat position ordering that ignores done.
func TestItemsOrdersUndoneBeforeDoneByPosition(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	a, _ := db.CreateItem(ctx, c.ID, owner.ID, "A", "")
	b, _ := db.CreateItem(ctx, c.ID, owner.ID, "B", "")
	cc, _ := db.CreateItem(ctx, c.ID, owner.ID, "C", "")
	d, _ := db.CreateItem(ctx, c.ID, owner.ID, "D", "")

	if _, err := db.ToggleItem(ctx, b.ID); err != nil {
		t.Fatalf("ToggleItem B: %v", err)
	}
	if _, err := db.ToggleItem(ctx, d.ID); err != nil {
		t.Fatalf("ToggleItem D: %v", err)
	}

	items, err := db.Items(ctx, c.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	want := []int64{a.ID, cc.ID, b.ID, d.ID}
	if len(items) != len(want) {
		t.Fatalf("Items returned %d rows, want %d", len(items), len(want))
	}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("Items()[%d].ID = %d, want %d (full order: %+v)", i, items[i].ID, id, items)
		}
	}
}

// TestToggleDoesNotChangePosition is the property that makes a tick a pure
// consequence of the ORDER BY rather than a data rewrite: unticking the item
// again must bring it back to exactly where it was among the undone items,
// which only holds if toggling never touched position in the first place.
func TestToggleDoesNotChangePosition(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := db.CreateItem(ctx, c.ID, owner.ID, "Milk", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	before := positionOf(t, db, item.ID)
	if _, err := db.ToggleItem(ctx, item.ID); err != nil {
		t.Fatalf("ToggleItem: %v", err)
	}
	after := positionOf(t, db, item.ID)
	if before != after {
		t.Fatalf("position changed on toggle: %d -> %d", before, after)
	}
}

// TestCreateItemLandsAboveDoneItems shows why a new item's position — one
// past the current maximum across the whole collection, done items included
// — still ends up above any already-done item on the rendered list: Items'
// done-first sort key wins regardless of the numeric position value.
func TestCreateItemLandsAboveDoneItems(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	a, _ := db.CreateItem(ctx, c.ID, owner.ID, "A", "")
	b, _ := db.CreateItem(ctx, c.ID, owner.ID, "B", "")
	cc, _ := db.CreateItem(ctx, c.ID, owner.ID, "C", "")
	if _, err := db.ToggleItem(ctx, b.ID); err != nil {
		t.Fatalf("ToggleItem B: %v", err)
	}

	d, err := db.CreateItem(ctx, c.ID, owner.ID, "D", "")
	if err != nil {
		t.Fatalf("CreateItem D: %v", err)
	}

	items, err := db.Items(ctx, c.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	want := []int64{a.ID, cc.ID, d.ID, b.ID}
	if len(items) != len(want) {
		t.Fatalf("Items returned %d rows, want %d", len(items), len(want))
	}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("Items()[%d].ID = %d, want %d (full order: %+v)", i, items[i].ID, id, items)
		}
	}
}

// TestReorderItemsAppliesGivenOrder is the basic drag-and-drop path: the
// order passed in becomes the order Items reads back.
func TestReorderItemsAppliesGivenOrder(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	owner, err := db.UserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, owner.ID, "Groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	a, _ := db.CreateItem(ctx, c.ID, owner.ID, "A", "")
	b, _ := db.CreateItem(ctx, c.ID, owner.ID, "B", "")
	cc, _ := db.CreateItem(ctx, c.ID, owner.ID, "C", "")

	if err := db.ReorderItems(ctx, c.ID, []int64{cc.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("ReorderItems: %v", err)
	}

	items, err := db.Items(ctx, c.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	want := []int64{cc.ID, a.ID, b.ID}
	if len(items) != len(want) {
		t.Fatalf("Items returned %d rows, want %d", len(items), len(want))
	}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("Items()[%d].ID = %d, want %d (full order: %+v)", i, items[i].ID, id, items)
		}
	}
}

// TestReorderItemsCannotMoveItemBetweenCollections is the security property:
// an id smuggled in from a collection the caller does not control over must
// not be adopted into the caller's collection. This mirrors the item-id
// smuggling protection itemFor enforces in the web layer, but at the store
// layer, where ReorderItems' WHERE clause is the only thing standing between
// an arbitrary id and someone else's data.
func TestReorderItemsCannotMoveItemBetweenCollections(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	ownerA, err := db.UserByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("UserByEmail A: %v", err)
	}
	collA, err := db.CreateCollection(ctx, ownerA.ID, "A's list")
	if err != nil {
		t.Fatalf("CreateCollection A: %v", err)
	}
	itemA, err := db.CreateItem(ctx, collA.ID, ownerA.ID, "mine", "")
	if err != nil {
		t.Fatalf("CreateItem A: %v", err)
	}

	ownerB, err := db.UserByEmail(ctx, "b@example.com")
	if err != nil {
		t.Fatalf("UserByEmail B: %v", err)
	}
	collB, err := db.CreateCollection(ctx, ownerB.ID, "B's list")
	if err != nil {
		t.Fatalf("CreateCollection B: %v", err)
	}
	itemB, err := db.CreateItem(ctx, collB.ID, ownerB.ID, "theirs", "")
	if err != nil {
		t.Fatalf("CreateItem B: %v", err)
	}

	beforeA := positionOf(t, db, itemA.ID)
	beforeB := positionOf(t, db, itemB.ID)

	// Reorder collection A but pass B's item id. The WHERE id = ? AND
	// collection_id = ? clause should match nothing for it, and the call
	// must still succeed rather than erroring on the zero-row update.
	if err := db.ReorderItems(ctx, collA.ID, []int64{itemB.ID}); err != nil {
		t.Fatalf("ReorderItems with a foreign id: %v", err)
	}

	if got := positionOf(t, db, itemB.ID); got != beforeB {
		t.Fatalf("B's item position changed via a reorder scoped to A's collection: %d -> %d", beforeB, got)
	}
	if got := positionOf(t, db, itemA.ID); got != beforeA {
		t.Fatalf("A's item position changed even though it was not in the reorder list: %d -> %d", beforeA, got)
	}

	// B's collection is untouched — the item did not get pulled into A's.
	itemsB, err := db.Items(ctx, collB.ID)
	if err != nil {
		t.Fatalf("Items B: %v", err)
	}
	if len(itemsB) != 1 || itemsB[0].ID != itemB.ID {
		t.Fatalf("collection B's items changed: %+v", itemsB)
	}
}

func TestErrNotFound(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if _, err := db.Collection(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Collection(missing) = %v, want ErrNotFound", err)
	}
	if _, err := db.Item(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Item(missing) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteCollection(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteCollection(missing) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteItem(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteItem(missing) = %v, want ErrNotFound", err)
	}
}
