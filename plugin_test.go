package slugify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
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

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         32,
		},
	})

	p := &Plugin{}

	err := p.Init(app)
	if err != nil {
		t.Fatalf("expected init to succeed and skip invalid config, got %v", err)
	}
}

func TestInitFailsForDuplicateCollectionAndOutputTarget(t *testing.T) {
	app := newTestApp(t)

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
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
	})

	p := &Plugin{}

	err := p.Init(app)
	if err != nil {
		t.Fatalf("expected init to succeed and skip duplicate config, got %v", err)
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

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title", "slug"},
			OutputField:    "slug",
			Length:         32,
		},
	})

	p := &Plugin{}

	err := p.Init(app)
	if err != nil {
		t.Fatalf("expected init to succeed and skip invalid config, got %v", err)
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

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         32,
		},
	})

	p := &Plugin{}

	err := p.Init(app)
	if err != nil {
		t.Fatalf("expected init to succeed and skip invalid config, got %v", err)
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

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title", "subtitle"},
			OutputField:    "slug",
			Length:         18,
		},
	})

	p := &Plugin{}

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

	pluginsCollection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("expected %s collection to exist: %v", pluginsCollectionName, err)
	}

	pluginRecord, err := app.FindFirstRecordByFilter(pluginsCollection, pluginNameField+" = {:pluginName}", map[string]any{"pluginName": "slugify"})
	if err != nil {
		t.Fatalf("expected slugify plugin record: %v", err)
	}

	if !pluginRecord.GetBool(pluginEnabledField) {
		t.Fatal("expected slugify plugin record to be enabled")
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

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "pages",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         20,
		},
	})

	p := &Plugin{}

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

func TestInitCreatesPluginsCollectionWithoutConfig(t *testing.T) {
	app := newTestApp(t)

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("expected %s collection to exist: %v", pluginsCollectionName, err)
	}

	if collection.Fields.GetByName(pluginNameField) == nil {
		t.Fatalf("expected %s field to exist", pluginNameField)
	}

	if collection.Fields.GetByName(pluginConfigField) == nil {
		t.Fatalf("expected %s field to exist", pluginConfigField)
	}

	if collection.Fields.GetByName(pluginEnabledField) == nil {
		t.Fatalf("expected %s field to exist", pluginEnabledField)
	}
}

func TestInitDefersStorageSetupUntilBootstrap(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "pb_data")
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir: dataDir,
	})

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin before bootstrap: %v", err)
	}

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

	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("expected %s collection to exist after bootstrap: %v", pluginsCollectionName, err)
	}

	if collection.Fields.GetByName(pluginNameField) == nil {
		t.Fatalf("expected %s field to exist after bootstrap", pluginNameField)
	}
}

func TestPluginReloadsConfigWhenPluginsRecordChanges(t *testing.T) {
	app := newTestApp(t)

	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)
	posts.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(posts); err != nil {
		t.Fatalf("failed to save posts collection: %v", err)
	}

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	record := core.NewRecord(posts)
	record.Set("title", "Before Config")
	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save pre-config record: %v", err)
	}
	if got := record.GetString("slug"); got != "" {
		t.Fatalf("expected no slug before plugin config, got %q", got)
	}

	pluginRecord := createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         12,
		},
	})

	afterCreate := core.NewRecord(posts)
	afterCreate.Set("title", "After Config")
	if err := app.Save(afterCreate); err != nil {
		t.Fatalf("failed to save post-config record: %v", err)
	}
	if got := afterCreate.GetString("slug"); got != "after-config" {
		t.Fatalf("unexpected slug after config create: got %q want %q", got, "after-config")
	}

	pluginRecord.Set(pluginEnabledField, false)
	if err := app.Save(pluginRecord); err != nil {
		t.Fatalf("failed to disable plugin config: %v", err)
	}

	afterDisable := core.NewRecord(posts)
	afterDisable.Set("title", "After Disable")
	if err := app.Save(afterDisable); err != nil {
		t.Fatalf("failed to save disabled record: %v", err)
	}
	if got := afterDisable.GetString("slug"); got != "" {
		t.Fatalf("expected no slug while disabled, got %q", got)
	}

	pluginRecord.Set(pluginEnabledField, true)
	pluginRecord.SetRaw(pluginConfigField, mustJSONRaw(t, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         8,
		},
	}))
	if err := app.Save(pluginRecord); err != nil {
		t.Fatalf("failed to update plugin config: %v", err)
	}

	afterUpdate := core.NewRecord(posts)
	afterUpdate.Set("title", "Longer Title")
	if err := app.Save(afterUpdate); err != nil {
		t.Fatalf("failed to save updated-config record: %v", err)
	}
	if got := afterUpdate.GetString("slug"); got != "longer-t" {
		t.Fatalf("unexpected slug after config update: got %q want %q", got, "longer-t")
	}

	if err := app.Delete(pluginRecord); err != nil {
		t.Fatalf("failed to delete plugin config: %v", err)
	}

	afterDelete := core.NewRecord(posts)
	afterDelete.Set("title", "After Delete")
	if err := app.Save(afterDelete); err != nil {
		t.Fatalf("failed to save deleted-config record: %v", err)
	}
	if got := afterDelete.GetString("slug"); got != "" {
		t.Fatalf("expected no slug after config delete, got %q", got)
	}
}

func TestPluginClearsActiveConfigOnInvalidReload(t *testing.T) {
	app := newTestApp(t)

	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)
	posts.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(posts); err != nil {
		t.Fatalf("failed to save posts collection: %v", err)
	}

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	pluginRecord := createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         20,
		},
	})

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	first := core.NewRecord(posts)
	first.Set("title", "Before Break")
	if err := app.Save(first); err != nil {
		t.Fatalf("failed to save first record: %v", err)
	}
	if got := first.GetString("slug"); got != "before-break" {
		t.Fatalf("unexpected slug before invalid reload: got %q want %q", got, "before-break")
	}

	pluginRecord.SetRaw(pluginConfigField, mustJSONRaw(t, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"missing_field"},
			OutputField:    "slug",
			Length:         20,
		},
	}))
	if err := app.Save(pluginRecord); err != nil {
		t.Fatalf("failed to save invalid plugin config: %v", err)
	}

	second := core.NewRecord(posts)
	second.Set("title", "After Break")
	if err := app.Save(second); err != nil {
		t.Fatalf("failed to save second record: %v", err)
	}
	if got := second.GetString("slug"); got != "" {
		t.Fatalf("expected config to be cleared after invalid reload, got slug %q", got)
	}
}

func TestPluginSkipsInvalidRulesButKeepsValidOnes(t *testing.T) {
	app := newTestApp(t)

	pages := core.NewBaseCollection("pages")
	pages.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
		&core.TextField{Name: "slug2"},
	)
	pages.AddIndex("idx_pages_slug_unique", true, "`slug`", "`slug` != ''")
	pages.AddIndex("idx_pages_slug2_unique", true, "`slug2`", "`slug2` != ''")

	if err := app.Save(pages); err != nil {
		t.Fatalf("failed to save pages collection: %v", err)
	}

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "pages",
			InputFields:    []string{"missing_field"},
			OutputField:    "slug2",
			Length:         20,
		},
		{
			CollectionName: "pages",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         20,
		},
	})

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	record := core.NewRecord(pages)
	record.Set("title", "Valid Rule")
	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record: %v", err)
	}
	if got := record.GetString("slug"); got != "valid-rule" {
		t.Fatalf("unexpected slug with mixed valid/invalid config rules: got %q want %q", got, "valid-rule")
	}
}

func TestPluginReloadsWhenCollectionIsCreatedLater(t *testing.T) {
	app := newTestApp(t)

	if err := ensurePluginsCollection(app); err != nil {
		t.Fatalf("failed to ensure plugins collection: %v", err)
	}

	createPluginConfigRecord(t, app, true, []SlugConfig{
		{
			CollectionName: "posts",
			InputFields:    []string{"title"},
			OutputField:    "slug",
			Length:         20,
		},
	})

	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatalf("failed to init plugin: %v", err)
	}

	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "slug"},
	)
	posts.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")

	if err := app.Save(posts); err != nil {
		t.Fatalf("failed to save posts collection: %v", err)
	}

	record := core.NewRecord(posts)
	record.Set("title", "Created Later")
	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save record after collection create: %v", err)
	}
	if got := record.GetString("slug"); got != "created-later" {
		t.Fatalf("unexpected slug after collection-triggered reload: got %q want %q", got, "created-later")
	}
}

func createPluginConfigRecord(t *testing.T, app *core.BaseApp, enabled bool, configs []SlugConfig) *core.Record {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		t.Fatalf("failed to load %s collection: %v", pluginsCollectionName, err)
	}

	record := core.NewRecord(collection)
	record.Set(pluginNameField, "slugify")
	record.SetRaw(pluginConfigField, mustJSONRaw(t, configs))
	record.Set(pluginEnabledField, enabled)

	if err := app.Save(record); err != nil {
		t.Fatalf("failed to save plugin config record: %v", err)
	}

	return record
}

func mustJSONRaw(t *testing.T, value any) types.JSONRaw {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to marshal json: %v", err)
	}

	return types.JSONRaw(data)
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
