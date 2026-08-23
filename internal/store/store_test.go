package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
