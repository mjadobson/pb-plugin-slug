package slugify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func TestBuildSlugNormalizesAndTruncates(t *testing.T) {
	if got, want := buildSlug([]string{"Crème brûlée", "A&B"}, 16), "creme-brulee-a-b"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPluginMetadataForBaseRegistry(t *testing.T) {
	p := &Plugin{}
	if p.Name() != "slugify" || p.Label() != "Slugify" || p.Author() != "mjadobson" || p.Icon() == "" {
		t.Fatal("unexpected plugin metadata")
	}
}

func TestInitCreatesSystemConfigCollection(t *testing.T) {
	app := newTestApp(t)
	if err := (&Plugin{}).Init(app); err != nil {
		t.Fatal(err)
	}
	collection, err := app.FindCollectionByNameOrId(configsCollectionName)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.System {
		t.Fatal("config collection must be a system collection")
	}
	for _, name := range []string{"collection_name", "input_fields", "output_field", "length", "recalculating", recalculationErrorField} {
		if collection.Fields.GetByName(name) == nil {
			t.Fatalf("missing config field %q", name)
		}
	}
	length, ok := collection.Fields.GetByName("length").(*core.NumberField)
	if !ok || !length.OnlyInt || length.Min == nil || *length.Min != 1 {
		t.Fatal("length must be a positive integer field")
	}
}

func TestInitRejectsIncompatibleConfigCollection(t *testing.T) {
	app := newTestApp(t)
	collection := core.NewBaseCollection(configsCollectionName)
	collection.Fields.Add(&core.TextField{Name: "unexpected"})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	err := (&Plugin{}).Init(app)
	if err == nil || !strings.Contains(err.Error(), "system base collection") {
		t.Fatalf("expected incompatible collection error, got %v", err)
	}
}

func TestRuleValidationRejectsInvalidTarget(t *testing.T) {
	app := newTestApp(t)
	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "slug"})
	if err := app.Save(posts); err != nil {
		t.Fatal(err)
	}
	if err := (&Plugin{}).Init(app); err != nil {
		t.Fatal(err)
	}

	collection, _ := app.FindCollectionByNameOrId(configsCollectionName)
	rule := core.NewRecord(collection)
	rule.Set("collection_name", "posts")
	rule.Set("input_fields", []string{"title"})
	rule.Set("output_field", "slug")
	rule.Set("length", 64)
	if err := app.Save(rule); err == nil {
		t.Fatal("expected rule without a unique output index to be rejected")
	}
}

func TestLegacyMigrationRollsBackAllRowsOnFailure(t *testing.T) {
	app := newTestApp(t)
	newSlugTarget(t, app, "posts")
	if err := ensureConfigsCollection(app); err != nil {
		t.Fatal(err)
	}
	createLegacyConfig(t, app, []map[string]any{
		{"collection_name": "posts", "input_fields": []string{"title"}, "output_field": "slug", "length": 64},
		{"collection_name": "posts", "input_fields": []string{"title"}, "output_field": "slug", "length": 32},
	})

	if err := migrateLegacyConfigs(app); err == nil {
		t.Fatal("expected duplicate migration to fail")
	}
	records, err := app.FindAllRecords(configsCollectionName)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("migration was not atomic: found %d row(s)", len(records))
	}
}

func TestRulesAreRowsAndReloadOnCRUD(t *testing.T) {
	app := newTestApp(t)
	posts := newSlugTarget(t, app, "posts")
	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatal(err)
	}
	rule := createRule(t, app, "posts", []string{"title"}, "slug", 12, false)
	record := core.NewRecord(posts)
	record.Set("title", "A Longer Title")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	if got := record.GetString("slug"); got != "a-longer-tit" {
		t.Fatalf("got slug %q", got)
	}
	if err := app.Delete(rule); err != nil {
		t.Fatal(err)
	}
	withoutRule := core.NewRecord(posts)
	withoutRule.Set("title", "No Slug")
	if err := app.Save(withoutRule); err != nil {
		t.Fatal(err)
	}
	if withoutRule.GetString("slug") != "" {
		t.Fatal("deleted rule remained active")
	}
}

func TestSlugGenerationKeepsUniquenessAndClearsBlankInputs(t *testing.T) {
	app := newTestApp(t)
	posts := newSlugTarget(t, app, "posts")
	if err := (&Plugin{}).Init(app); err != nil {
		t.Fatal(err)
	}
	createRule(t, app, "posts", []string{"title"}, "slug", 12, false)

	first := core.NewRecord(posts)
	first.Set("title", "A Longer Title")
	if err := app.Save(first); err != nil {
		t.Fatal(err)
	}
	second := core.NewRecord(posts)
	second.Set("title", "A Longer Title")
	if err := app.Save(second); err != nil {
		t.Fatal(err)
	}
	if first.GetString("slug") != "a-longer-tit" || second.GetString("slug") != "a-longer-t-1" {
		t.Fatalf("unexpected unique slugs %q and %q", first.GetString("slug"), second.GetString("slug"))
	}

	first.Set("title", "   ")
	if err := app.Save(first); err != nil {
		t.Fatal(err)
	}
	if first.GetString("slug") != "" {
		t.Fatalf("blank input did not clear slug: %q", first.GetString("slug"))
	}
}

func TestSlugGenerationSupportsArraysAndOneLevelRelations(t *testing.T) {
	app := newTestApp(t)
	docs := core.NewBaseCollection("docs")
	docs.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(docs); err != nil {
		t.Fatal(err)
	}
	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(
		&core.TextField{Name: "title"},
		&core.SelectField{Name: "tags", Values: []string{"red", "blue"}, MaxSelect: 2},
		&core.RelationField{Name: "docs", CollectionId: docs.Id, MaxSelect: 2},
		&core.TextField{Name: "slug"},
	)
	posts.AddIndex("idx_posts_slug_unique", true, "`slug`", "`slug` != ''")
	if err := app.Save(posts); err != nil {
		t.Fatal(err)
	}
	if err := (&Plugin{}).Init(app); err != nil {
		t.Fatal(err)
	}
	createRule(t, app, "posts", []string{"docs.name", "title", "tags"}, "slug", 64, false)

	doc := core.NewRecord(docs)
	doc.Set("name", "Alpha Guide")
	if err := app.Save(doc); err != nil {
		t.Fatal(err)
	}
	post := core.NewRecord(posts)
	post.Set("docs", []string{doc.Id})
	post.Set("title", "Palette")
	post.Set("tags", []string{"red", "blue"})
	if err := app.Save(post); err != nil {
		t.Fatal(err)
	}
	if got, want := post.GetString("slug"), "alpha-guide-palette-red-blue"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecalculationUsesRecordStatusAndClearsIt(t *testing.T) {
	app := newTestApp(t)
	posts := newSlugTarget(t, app, "posts")
	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		record := core.NewRecord(posts)
		record.Set("title", fmt.Sprintf("Post %d", i))
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	rule := createRule(t, app, "posts", []string{"title"}, "slug", 64, true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		refreshed, err := app.FindRecordById(configsCollectionName, rule.Id)
		if err != nil {
			t.Fatal(err)
		}
		if !refreshed.GetBool("recalculating") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recalculation status was not cleared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	records, err := app.FindAllRecords(posts.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.GetString("slug") == "" {
			t.Fatalf("record %s was not recalculated", record.Id)
		}
	}
}

func TestRecalculationProcessesMoreThanOneBatch(t *testing.T) {
	app := newTestApp(t)
	posts := newSlugTarget(t, app, "posts")
	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < recalculateBatchSize+5; i++ {
		record := core.NewRecord(posts)
		record.Set("title", fmt.Sprintf("Post %d", i))
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	rule := createRule(t, app, "posts", []string{"title"}, "slug", 64, true)
	waitForRecalculation(t, app, rule.Id)

	records, err := app.FindAllRecords(posts.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != recalculateBatchSize+5 {
		t.Fatalf("got %d records", len(records))
	}
	for _, record := range records {
		if record.GetString("slug") == "" {
			t.Fatalf("record %s was skipped", record.Id)
		}
	}
}

func TestRecalculationPersistsFailureForAdminToast(t *testing.T) {
	app := newTestApp(t)
	posts := newSlugTarget(t, app, "posts")
	app.OnRecordUpdate(posts.Name).BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetRaw(slugifyInProgressKey) == true {
			return errors.New("forced recalc failure")
		}
		return e.Next()
	})
	p := &Plugin{}
	if err := p.Init(app); err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(posts)
	record.Set("title", "Break me")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	rule := createRule(t, app, "posts", []string{"title"}, "slug", 64, true)
	refreshed := waitForRecalculation(t, app, rule.Id)
	if got := refreshed.GetString(recalculationErrorField); !strings.Contains(got, "forced recalc failure") {
		t.Fatalf("expected persisted recalculation error, got %q", got)
	}
}

func newSlugTarget(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	collection := core.NewBaseCollection(name)
	collection.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "slug"})
	collection.AddIndex("idx_"+name+"_slug_unique", true, "`slug`", "`slug` != ''")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	return collection
}

func createRule(t *testing.T, app core.App, collection string, inputs []string, output string, length int, recalculating bool) *core.Record {
	t.Helper()
	configCollection, err := app.FindCollectionByNameOrId(configsCollectionName)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(configCollection)
	record.Set("collection_name", collection)
	record.Set("input_fields", inputs)
	record.Set("output_field", output)
	record.Set("length", length)
	record.Set("recalculating", recalculating)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func createLegacyConfig(t *testing.T, app core.App, configs any) {
	t.Helper()
	collection := core.NewBaseCollection("_plugins")
	collection.Fields.Add(
		&core.TextField{Name: "plugin_name", Required: true},
		&core.JSONField{Name: "config", Required: true},
		&core.BoolField{Name: "enabled"},
	)
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(collection)
	record.Set("plugin_name", "slugify")
	record.Set("config", string(data))
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
}

func waitForRecalculation(t *testing.T, app core.App, id string) *core.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err := app.FindRecordById(configsCollectionName, id)
		if err != nil {
			t.Fatal(err)
		}
		if !record.GetBool("recalculating") {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatal("recalculation status was not cleared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestApp(t *testing.T) *core.BaseApp {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "pb_data")
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dataDir})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.ResetBootstrapState(); err != nil {
			t.Error(err)
		}
		_ = os.RemoveAll(dataDir)
	})
	return app
}
