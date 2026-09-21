package repository

import (
	"context"
	"sort"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// wikiPagesSourceRefDDL is the minimal subset of wiki_pages columns that
// ListSlugsBySourceRef touches (knowledge_base_id filter + source_refs
// predicate + slug projection). Kept narrow so the test exercises exactly the
// cross-dialect predicate without depending on the full model.
const wikiPagesSourceRefDDL = `
CREATE TABLE IF NOT EXISTS wiki_pages (
    id VARCHAR(36) PRIMARY KEY,
    knowledge_base_id VARCHAR(36) NOT NULL,
    slug VARCHAR(255) NOT NULL,
    source_refs TEXT,
    deleted_at DATETIME
);
`

func newWikiPageSourceRefDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.New().String() + "?mode=memory&cache=shared&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Exec(wikiPagesSourceRefDDL).Error)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestListSlugsBySourceRef_SQLite is the regression guard for the Lite-mode
// bug where ListSlugsBySourceRef (and its siblings) emitted Postgres-only SQL
// (`source_refs @> ?::jsonb OR source_refs::text LIKE ?`). On SQLite the `@>`
// operator and `::` casts are invalid, so the wiki ingest pipeline's existing-
// page lookup failed with "unrecognized token: \"@\"" — silently breaking edge
// building / retract. The fix branches on the dialector. This test runs against
// real SQLite and asserts it both (a) does not error and (b) matches the legacy
// bare-id form AND the current "id|title" form, while excluding other ids.
func TestListSlugsBySourceRef_SQLite(t *testing.T) {
	db := newWikiPageSourceRefDB(t)
	repo := NewWikiPageRepository(db)

	const kb = "kb-1"
	kid := uuid.New().String()
	other := uuid.New().String()

	seed := []struct {
		slug string
		refs string
	}{
		{"s-old", `["` + kid + `"]`},
		{"s-new", `["` + kid + `|Some 中文 Title"]`},
		{"s-multi", `["` + other + `","` + kid + `|X"]`},
		{"s-other", `["` + other + `"]`},
	}
	for _, s := range seed {
		require.NoError(t, db.Exec(
			"INSERT INTO wiki_pages (id, knowledge_base_id, slug, source_refs) VALUES (?,?,?,?)",
			uuid.New().String(), kb, s.slug, s.refs,
		).Error)
	}

	got, err := repo.ListSlugsBySourceRef(context.Background(), kb, kid)
	require.NoError(t, err, "ListSlugsBySourceRef must not error on SQLite (was: unrecognized token \"@\")")
	sort.Strings(got)
	require.Equal(t, []string{"s-multi", "s-new", "s-old"}, got,
		"must match both bare-id and id|title forms, exclude unrelated ids")

	// A knowledge id with no references returns empty, not an error.
	none, err := repo.ListSlugsBySourceRef(context.Background(), kb, uuid.New().String())
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestListBySourceRef_SQLiteKeepsSourceRefs guards the second half of the Lite
// bug that the predicate fix alone did not solve. Matching the row on SQLite is
// not enough: ListBySourceRef also reads each page's source_refs back, SQLite
// hands TEXT to the driver as Go string, and StringArray.Scan used to accept
// only []byte. The row matched the predicate yet arrived with empty SourceRefs,
// so the wiki cleanup path — which deletes a page once no sources remain — saw
// "nothing else references this page" and destroyed pages other knowledge files
// still shared.
//
// The rows are inserted as TEXT on purpose. GORM binds a StringArray as a BLOB,
// which the driver reads back as []byte and which the old code handled; TEXT is
// what the column default ('[]') and any raw-SQL writer produce, so a GORM-only
// fixture exercises none of this.
func TestListBySourceRef_SQLiteKeepsSourceRefs(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	const kb = "kb-src"
	kid := uuid.New().String()
	other := uuid.New().String()
	otherRef := other + "|Other Doc"
	kidRef := kid + "|My Doc"

	seed := []struct {
		slug string
		refs string
	}{
		{"summary/shared", `["` + otherRef + `","` + kidRef + `"]`},
		{"summary/sole", `["` + kid + `"]`},
		{"summary/unrelated", `["` + other + `"]`},
	}
	for _, s := range seed {
		require.NoError(t, db.Exec(
			"INSERT INTO wiki_pages (id, tenant_id, knowledge_base_id, slug, source_refs) VALUES (?,?,?,?,?)",
			uuid.New().String(), 1, kb, s.slug, s.refs,
		).Error)
	}

	pages, err := repo.ListBySourceRef(ctx, kb, kid)
	require.NoError(t, err)
	require.Len(t, pages, 2, "the shared and the sole page both reference the id")

	bySlug := make(map[string]*types.WikiPage, len(pages))
	for _, p := range pages {
		bySlug[p.Slug] = p
	}

	require.Equal(t, types.StringArray{otherRef, kidRef}, bySlug["summary/shared"].SourceRefs,
		"source_refs must survive the SQLite round-trip; losing the other ref makes cleanup delete a shared page")
	require.Equal(t, types.StringArray{kid}, bySlug["summary/sole"].SourceRefs)
}
