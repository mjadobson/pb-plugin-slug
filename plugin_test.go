package slugify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestBuildSlugNormalizesAndTruncates(t *testing.T) {
	got := buildSlug([]string{"Hello, World!", "Cafe deja vu"}, 16)
	want := "hello-world-cafe"

	if got != want {
		t.Fatalf("unexpected slug: got %q want %q", got, want)
	}
}

func TestBuildSlugRemovesAccentsAndExtraSeparators(t *testing.T) {
	got := buildSlug([]string{"  Crème   brûlée ", "A&B"}, 64)
	want := "creme-brulee-a-b"

	if got != want {
		t.Fatalf("unexpected slug: got %q want %q", got, want)
	}
}

func TestPluginMetadata(t *testing.T) {
	p := &Plugin{}

	if p.Name() != "slugify" {
		t.Fatalf("unexpected plugin name: %q", p.Name())
	}

	if p.Description() == "" {
		t.Fatal("expected plugin description to be non-empty")
	}

	if p.Version() == "" {
		t.Fatal("expected plugin version to be non-empty")
	}
}

func TestInitFailsForInvalidExistingOutputFieldType(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("posts")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "slug"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "posts",
				InputFields:    []string{"title"},
				OutputField:    "slug",
				Length:         32,
			},
		},
	}

	err := p.Init(app)
	if err == nil {
		t.Fatal("expected init to fail for non-text output field")
	}
}

func TestInitFailsForDuplicateCollectionAndOutputTarget(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "posts",
				InputFields:    []string{"title"},
				OutputField:    "slug",
				Length:         32,
			},
			{
				CollectionName: "posts",
				InputFields:    []string{"subtitle"},
				OutputField:    "slug",
				Length:         32,
			},
		},
	}

	err := p.Init(app)
	if err == nil {
		t.Fatal("expected init to fail for duplicate collection/output config target")
	}
}

func TestInitFailsWhenOutputFieldIsAlsoAnInputField(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("posts")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)
	collection.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "posts",
				InputFields:    []string{"title", "slug"},
				OutputField:    "slug",
				Length:         32,
			},
		},
	}

	err := p.Init(app)
	if err == nil {
		t.Fatal("expected init to fail when output_field is also in input_fields")
	}
}

func TestInitFailsWithoutUniqueIndexOnOutputField(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("posts")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "posts",
				InputFields:    []string{"title"},
				OutputField:    "slug",
				Length:         32,
			},
		},
	}

	err := p.Init(app)
	if err == nil {
		t.Fatal("expected init to fail when output field has no unique index")
	}
}

func TestPluginPocketBaseIntegrationCreatesAndUpdatesSlugs(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("posts")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "subtitle"},
		&core.TextField{Name: "slug"},
	)
	collection.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "posts",
				InputFields:    []string{"title", "subtitle"},
				OutputField:    "slug",
				Length:         18,
			},
		},
	}

	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	first := core.NewRecord(collection)
	first.Set("title", "Hello World")
	first.Set("subtitle", "Again")

	if err := app.Save(first); err != nil {
		t.Fatalf("failed to save first record: %v", err)
	}

	if got := first.GetString("slug"); got != "hello-world-again" {
		t.Fatalf("unexpected first slug: got %q want %q", got, "hello-world-again")
	}

	second := core.NewRecord(collection)
	second.Set("title", "Hello World")
	second.Set("subtitle", "Again")

	if err := app.Save(second); err != nil {
		t.Fatalf("failed to save second record: %v", err)
	}

	if got := second.GetString("slug"); got != "hello-world-agai-1" {
		t.Fatalf("unexpected second slug: got %q want %q", got, "hello-world-agai-1")
	}

	first.Set("title", "Updated title")
	first.Set("subtitle", "2026")

	if err := app.Save(first); err != nil {
		t.Fatalf("failed to update first record: %v", err)
	}

	if got := first.GetString("slug"); got != "updated-title-2026" {
		t.Fatalf("unexpected updated slug: got %q want %q", got, "updated-title-2026")
	}
}

func TestPluginClearsSlugWhenInputsAreBlank(t *testing.T) {
	app := newTestApp(t)

	collection := core.NewBaseCollection("pages")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)
	collection.AddIndex("idx_pages_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to save collection: %v", err)
	}

	p := &Plugin{
		Configs: []SlugConfig{
			{
				CollectionName: "pages",
				InputFields:    []string{"title"},
				OutputField:    "slug",
				Length:         20,
			},
		},
	}

	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	record := core.NewRecord(collection)
	record.Set("title", "Landing Page")

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record: %v", err)
	}

	record.Set("title", "   ")

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to update record with blank title: %v", err)
	}

	if got := record.GetString("slug"); got != "" {
		t.Fatalf("expected blank slug after blank input, got %q", got)
	}
}

func newTestApp(t *testing.T) *core.BaseApp {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), "pb_data")
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir: dataDir,
	})

	if err := app.Bootstrap(); err != nil {
		t.Fatalf("failed to bootstrap pocketbase app: %v", err)
	}

	t.Cleanup(func() {
		if err := app.ResetBootstrapState(); err != nil {
			t.Fatalf("failed to reset pocketbase app: %v", err)
		}

		if err := os.RemoveAll(dataDir); err != nil {
			t.Fatalf("failed to remove pocketbase data dir: %v", err)
		}
	})

	return app
}
