package slugify

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/dbutils"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/pocketbuilds/xpb"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var version = "v0.4.0"

const slugifyInProgressKey = "@slugify_in_progress"
const maxUniqueSlugAttempts = 32
const recalculateBatchSize = 100
const configsCollectionName = "_plugins_slug"
const uiExtensionName = "pb-plugin-slug"
const recalculationErrorField = "recalculation_error"

var nonAlphaNumericPattern = regexp.MustCompile(`[^a-zA-Z0-9]+`)
var duplicateHyphenPattern = regexp.MustCompile(`-+`)

type SlugConfig struct {
	Id             string   `json:"id"`
	CollectionName string   `json:"collection_name"`
	InputFields    []string `json:"input_fields"`
	OutputField    string   `json:"output_field"`
	Length         int      `json:"length"`
	Recalculating  bool     `json:"recalculating,omitempty"`
}

type Plugin struct {
	mu               sync.RWMutex
	activeConfigs    []SlugConfig
	recalcInProgress map[string]struct{}
}

//go:embed all:ui
var embeddedUI embed.FS

func init() {
	xpb.Register(&Plugin{})
}

func (p *Plugin) Name() string {
	return "slugify"
}

func (p *Plugin) Version() string {
	return version
}

func (p *Plugin) Description() string {
	return "Generates normalized slugs from configured PocketBase record fields."
}

func (p *Plugin) Label() string { return "Slugify" }

func (p *Plugin) Author() string { return "mjadobson" }

func (p *Plugin) Icon() string { return "ri-links-line" }

func (p *Plugin) Init(app core.App) error {
	uiFS, err := fs.Sub(embeddedUI, "ui")
	if err != nil {
		return err
	}

	if app.IsBootstrapped() {
		if err := p.initialize(app); err != nil {
			return err
		}
	}

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		return p.initialize(e.App)
	})

	app.OnRecordAfterCreateSuccess().BindFunc(func(e *core.RecordEvent) error {
		if err := p.handleRecordEvent(e.Context, e.App, e.Record); err != nil {
			e.App.Logger().Error(
				"slugify create hook failed",
				slog.String("collection", e.Record.Collection().Name),
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	})

	app.OnRecordAfterUpdateSuccess().BindFunc(func(e *core.RecordEvent) error {
		if err := p.handleRecordEvent(e.Context, e.App, e.Record); err != nil {
			e.App.Logger().Error(
				"slugify update hook failed",
				slog.String("collection", e.Record.Collection().Name),
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.UIExtensions = append(e.UIExtensions, core.UIExtension{Name: uiExtensionName, FS: uiFS})
		return e.Next()
	})

	app.OnRecordValidate(configsCollectionName).BindFunc(func(e *core.RecordEvent) error {
		return validateConfigRecord(e)
	})

	reloadPluginsHook := func(e *core.RecordEvent) error {
		if err := p.reloadConfigs(e.App); err != nil {
			e.App.Logger().Error(
				"slugify config reload failed",
				slog.String("recordId", e.Record.Id),
				slog.Any("error", err),
			)
		}

		return e.Next()
	}

	app.OnRecordAfterCreateSuccess(configsCollectionName).BindFunc(reloadPluginsHook)
	app.OnRecordAfterUpdateSuccess(configsCollectionName).BindFunc(reloadPluginsHook)
	app.OnRecordAfterDeleteSuccess(configsCollectionName).BindFunc(reloadPluginsHook)

	reloadCollectionsHook := func(e *core.CollectionEvent) error {
		if err := p.reloadConfigs(e.App); err != nil {
			e.App.Logger().Error(
				"slugify config reload after collection change failed",
				slog.String("collection", e.Collection.Name),
				slog.Any("error", err),
			)
		}

		return e.Next()
	}

	app.OnCollectionAfterCreateSuccess().BindFunc(reloadCollectionsHook)
	app.OnCollectionAfterUpdateSuccess().BindFunc(reloadCollectionsHook)
	app.OnCollectionAfterDeleteSuccess().BindFunc(reloadCollectionsHook)

	return nil
}

func (p *Plugin) initialize(app core.App) error {
	if err := ensureConfigsCollection(app); err != nil {
		return err
	}
	if err := migrateLegacyConfigs(app); err != nil {
		return err
	}

	if err := p.reloadConfigs(app); err != nil {
		return err
	}

	return nil
}

func migrateLegacyConfigs(app core.App) error {
	existing, err := app.FindAllRecords(configsCollectionName)
	if err != nil || len(existing) > 0 {
		return err
	}
	legacyCollection, err := app.FindCollectionByNameOrId("_plugins")
	if err != nil {
		return nil
	}
	legacy, err := app.FindFirstRecordByFilter(legacyCollection, "plugin_name = {:name}", map[string]any{"name": "slugify"})
	if err != nil {
		if errorsIsNotFound(err) {
			return nil
		}
		return err
	}
	if !legacy.GetBool("enabled") {
		return nil
	}

	var configs []struct {
		CollectionName string   `json:"collection_name"`
		InputFields    []string `json:"input_fields"`
		OutputField    string   `json:"output_field"`
		Length         int      `json:"length"`
		Recalculate    bool     `json:"recalculate"`
		Recalculating  bool     `json:"recalculating"`
	}
	if err := json.Unmarshal([]byte(legacy.GetString("config")), &configs); err != nil {
		return fmt.Errorf("decode legacy slugify config: %w", err)
	}
	collection, err := app.FindCollectionByNameOrId(configsCollectionName)
	if err != nil {
		return err
	}
	return app.RunInTransaction(func(txApp core.App) error {
		txCollection, err := txApp.FindCollectionByNameOrId(collection.Id)
		if err != nil {
			return err
		}
		for _, cfg := range configs {
			record := core.NewRecord(txCollection)
			record.Set("collection_name", cfg.CollectionName)
			record.Set("input_fields", cfg.InputFields)
			record.Set("output_field", cfg.OutputField)
			record.Set("length", cfg.Length)
			record.Set("recalculating", cfg.Recalculate || cfg.Recalculating)
			if err := txApp.Save(record); err != nil {
				return fmt.Errorf("migrate legacy slugify config: %w", err)
			}
		}
		return nil
	})
}

func validateConfigRecord(e *core.RecordEvent) error {
	err := e.Next()
	errs := validation.Errors{}
	if validationErrs, ok := err.(validation.Errors); ok {
		errs = validationErrs
	} else if err != nil {
		return err
	}

	cfg := configFromRecord(e.Record)
	if cfg.CollectionName == "" {
		errs["collection_name"] = validation.NewError("validation_required", "Collection is required.")
	}
	if len(cfg.InputFields) == 0 {
		errs["input_fields"] = validation.NewError("validation_required", "Select at least one input field.")
	}
	if cfg.OutputField == "" {
		errs["output_field"] = validation.NewError("validation_required", "Output field is required.")
	}
	if cfg.Length <= 0 {
		errs["length"] = validation.NewError("validation_min", "Length must be greater than zero.")
	}
	if len(errs) == 0 {
		collection, findErr := e.App.FindCachedCollectionByNameOrId(cfg.CollectionName)
		if findErr != nil {
			errs["collection_name"] = validation.NewError("validation_not_found", "Collection does not exist.")
		} else if validateErr := validateConfigForCollection(e.App, cfg, collection); validateErr != nil {
			errs["input_fields"] = validation.NewError("validation_invalid_config", validateErr.Error())
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func validateConfigs(app core.App, pluginName string, configs []SlugConfig) []SlugConfig {
	if len(configs) == 0 {
		return nil
	}

	seenTargets := make(map[string]int, len(configs))
	validConfigs := make([]SlugConfig, 0, len(configs))

	for i, cfg := range configs {
		if cfg.CollectionName == "" || cfg.OutputField == "" || cfg.Length <= 0 || len(cfg.InputFields) == 0 {
			app.Logger().Warn(
				"slugify config skipped",
				slog.Int("configIndex", i),
				slog.String("plugin", pluginName),
				slog.Any("error", fmt.Errorf("%s: config %d must include collection_name, input_fields, output_field, and a positive length", pluginName, i)),
			)
			continue
		}

		key := cfg.CollectionName + "\x00" + cfg.OutputField
		if firstIndex, ok := seenTargets[key]; ok {
			app.Logger().Warn(
				"slugify config skipped",
				slog.Int("configIndex", i),
				slog.String("plugin", pluginName),
				slog.Any("error", fmt.Errorf("%s: config %d duplicates config %d for collection_name=%q and output_field=%q", pluginName, i, firstIndex, cfg.CollectionName, cfg.OutputField)),
			)
			continue
		}

		seenTargets[key] = i
		collection, err := app.FindCachedCollectionByNameOrId(cfg.CollectionName)
		if err != nil {
			app.Logger().Warn(
				"slugify config deferred",
				slog.Int("configIndex", i),
				slog.String("plugin", pluginName),
				slog.String("collection", cfg.CollectionName),
				slog.Any("error", fmt.Errorf("collection %q not found yet: %w", cfg.CollectionName, err)),
			)
			validConfigs = append(validConfigs, cfg)
			continue
		}

		if err := validateConfigForCollection(app, cfg, collection); err != nil {
			app.Logger().Warn(
				"slugify config skipped",
				slog.Int("configIndex", i),
				slog.String("plugin", pluginName),
				slog.String("collection", cfg.CollectionName),
				slog.Any("error", fmt.Errorf("%s: config %d invalid: %w", pluginName, i, err)),
			)
			continue
		}

		validConfigs = append(validConfigs, cfg)
	}

	return validConfigs
}

func ensureConfigsCollection(app core.App) error {
	collection, err := app.FindCollectionByNameOrId(configsCollectionName)
	isNew := false
	if err != nil {
		if !errorsIsNotFound(err) {
			return err
		}

		collection = core.NewBaseCollection(configsCollectionName)
		collection.System = true
		isNew = true
	}

	if isNew {
		collection.Fields.Add(&core.TextField{Name: "collection_name", Required: true})
		collection.Fields.Add(&core.JSONField{Name: "input_fields", Required: true})
		collection.Fields.Add(&core.TextField{Name: "output_field", Required: true})
		collection.Fields.Add(&core.NumberField{Name: "length", Required: true, Min: types.Pointer(1.0), OnlyInt: true})
		collection.Fields.Add(&core.BoolField{Name: "recalculating"})
		collection.Fields.Add(&core.TextField{Name: recalculationErrorField})
		collection.AddIndex("idx__plugins_slug_target_unique", true, "`collection_name`, `output_field`", "")
		if err := app.Save(collection); err != nil {
			return err
		}
	} else {
		if collection.Type != core.CollectionTypeBase || !collection.System {
			return fmt.Errorf("%s must be a system base collection", configsCollectionName)
		}

		needsSave := false
		if collection.Fields.GetByName(recalculationErrorField) == nil {
			// Schema addition introduced with asynchronous error reporting.
			collection.Fields.Add(&core.TextField{Name: recalculationErrorField})
			needsSave = true
		}
		if length, ok := collection.Fields.GetByName("length").(*core.NumberField); ok &&
			length.Required && length.Min != nil && *length.Min == 1 && !length.OnlyInt {
			// Upgrade collections created by the prerelease row-based config implementation.
			length.OnlyInt = true
			needsSave = true
		}
		if needsSave {
			if err := app.Save(collection); err != nil {
				return err
			}
		}
	}

	return verifyConfigsCollection(collection)
}

func verifyConfigsCollection(collection *core.Collection) error {
	if collection.Type != core.CollectionTypeBase || !collection.System {
		return fmt.Errorf("%s must be a system base collection", configsCollectionName)
	}

	type fieldCheck struct {
		name     string
		required bool
		valid    func(core.Field) bool
	}
	checks := []fieldCheck{
		{"collection_name", true, func(field core.Field) bool { _, ok := field.(*core.TextField); return ok }},
		{"input_fields", true, func(field core.Field) bool { _, ok := field.(*core.JSONField); return ok }},
		{"output_field", true, func(field core.Field) bool { _, ok := field.(*core.TextField); return ok }},
		{"length", true, func(field core.Field) bool {
			value, ok := field.(*core.NumberField)
			return ok && value.Min != nil && *value.Min == 1
		}},
		{"recalculating", false, func(field core.Field) bool { _, ok := field.(*core.BoolField); return ok }},
		{recalculationErrorField, false, func(field core.Field) bool { _, ok := field.(*core.TextField); return ok }},
	}
	for _, check := range checks {
		field := collection.Fields.GetByName(check.name)
		if field == nil || !check.valid(field) {
			return fmt.Errorf("%s has an incompatible %q field", configsCollectionName, check.name)
		}
		switch value := field.(type) {
		case *core.TextField:
			if value.Required != check.required {
				return fmt.Errorf("%s field %q has an incompatible required setting", configsCollectionName, check.name)
			}
		case *core.JSONField:
			if value.Required != check.required {
				return fmt.Errorf("%s field %q has an incompatible required setting", configsCollectionName, check.name)
			}
		case *core.NumberField:
			if value.Required != check.required || !value.OnlyInt {
				return fmt.Errorf("%s field %q must be a required integer", configsCollectionName, check.name)
			}
		case *core.BoolField:
			if value.Required != check.required {
				return fmt.Errorf("%s field %q has an incompatible required setting", configsCollectionName, check.name)
			}
		}
	}
	expectedFields := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		expectedFields[check.name] = struct{}{}
	}
	for _, field := range collection.Fields {
		if field.GetSystem() {
			continue
		}
		if _, ok := expectedFields[field.GetName()]; !ok {
			return fmt.Errorf("%s has unexpected field %q", configsCollectionName, field.GetName())
		}
	}
	indexFound := false
	for _, raw := range collection.Indexes {
		index := dbutils.ParseIndex(raw)
		if index.Unique && len(index.Columns) == 2 && strings.EqualFold(index.Columns[0].Name, "collection_name") && strings.EqualFold(index.Columns[1].Name, "output_field") {
			indexFound = true
			break
		}
	}
	if !indexFound {
		return fmt.Errorf("%s must have a unique collection_name/output_field index", configsCollectionName)
	}
	return nil
}

func (p *Plugin) reloadConfigs(app core.App) error {
	records, err := app.FindAllRecords(configsCollectionName)
	if err != nil {
		p.mu.Lock()
		p.activeConfigs = nil
		p.mu.Unlock()

		return err
	}

	configs := make([]SlugConfig, 0, len(records))
	for _, record := range records {
		configs = append(configs, configFromRecord(record))
	}

	p.mu.Lock()
	p.activeConfigs = slicesClone(validateConfigs(app, p.Name(), configs))
	p.mu.Unlock()

	for _, cfg := range configs {
		if cfg.Recalculating && p.beginRecalculation(cfg.Id) {
			go p.runRecalculation(app, cfg)
		}
	}

	return nil
}

func configFromRecord(record *core.Record) SlugConfig {
	return SlugConfig{
		Id: record.Id, CollectionName: record.GetString("collection_name"),
		InputFields: record.GetStringSlice("input_fields"), OutputField: record.GetString("output_field"),
		Length: record.GetInt("length"), Recalculating: record.GetBool("recalculating"),
	}
}

func (p *Plugin) handleRecordEvent(ctx context.Context, app core.App, record *core.Record) error {
	if record.Collection().Name == configsCollectionName {
		return nil
	}

	configs := p.getConfigsForRecord(record)
	for _, cfg := range configs {
		if err := processRecord(ctx, app, cfg, record); err != nil {
			app.Logger().Error(
				"slugify record processing failed",
				slog.String("collection", record.Collection().Name),
				slog.String("recordId", record.Id),
				slog.String("outputField", cfg.OutputField),
				slog.Any("error", err),
			)
		}
	}

	return nil
}

func (p *Plugin) getConfigsForRecord(record *core.Record) []SlugConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.activeConfigs) == 0 {
		return nil
	}

	result := make([]SlugConfig, 0, len(p.activeConfigs))
	for _, cfg := range p.activeConfigs {
		if cfg.CollectionName == record.Collection().Name || cfg.CollectionName == record.Collection().Id {
			result = append(result, cfg)
		}
	}

	return result
}

func slicesClone(configs []SlugConfig) []SlugConfig {
	if len(configs) == 0 {
		return nil
	}

	cloned := make([]SlugConfig, len(configs))
	copy(cloned, configs)
	return cloned
}

func errorsIsNotFound(err error) bool {
	return err != nil && (errors.Is(err, sql.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "no rows"))
}

func validateConfigForCollection(app core.App, cfg SlugConfig, collection *core.Collection) error {
	for _, fieldName := range cfg.InputFields {
		if fieldName == "" {
			return fmt.Errorf("input field names in collection %q cannot be empty", cfg.CollectionName)
		}

		if fieldName == cfg.OutputField {
			return fmt.Errorf("output field %q in collection %q cannot also be listed in input_fields", cfg.OutputField, cfg.CollectionName)
		}

		if err := validateInputFieldPath(app, cfg, collection, fieldName); err != nil {
			return err
		}
	}

	output := collection.Fields.GetByName(cfg.OutputField)
	if output == nil {
		return fmt.Errorf("output field %q not found in collection %q", cfg.OutputField, cfg.CollectionName)
	}

	if !isSupportedOutputField(output) {
		return fmt.Errorf("output field %q in collection %q must be a text or editor field", cfg.OutputField, cfg.CollectionName)
	}

	if _, ok := dbutils.FindSingleColumnUniqueIndex(collection.Indexes, cfg.OutputField); !ok {
		return fmt.Errorf("output field %q in collection %q must have a single-column UNIQUE index", cfg.OutputField, cfg.CollectionName)
	}

	return nil
}

func validateInputFieldPath(app core.App, cfg SlugConfig, collection *core.Collection, path string) error {
	segments := strings.Split(path, ".")
	currentCollection := collection

	for i, segment := range segments {
		if segment == "" {
			return fmt.Errorf("input field path %q in collection %q cannot contain empty segments", path, cfg.CollectionName)
		}

		field := currentCollection.Fields.GetByName(segment)
		if field == nil {
			return fmt.Errorf("input field %q not found in collection %q", segment, currentCollection.Name)
		}

		if i == len(segments)-1 {
			return nil
		}

		relationField, ok := field.(*core.RelationField)
		if !ok {
			return fmt.Errorf("input field path %q in collection %q must use relation fields before the final segment", path, cfg.CollectionName)
		}

		nextCollection, err := app.FindCachedCollectionByNameOrId(relationField.CollectionId)
		if err != nil {
			return fmt.Errorf("related collection %q for input field path %q in collection %q could not be loaded: %w", relationField.CollectionId, path, cfg.CollectionName, err)
		}

		currentCollection = nextCollection
	}

	return nil
}

func isSupportedOutputField(field core.Field) bool {
	switch field.(type) {
	case *core.TextField, *core.EditorField:
		return true
	default:
		return false
	}
}

func processRecord(ctx context.Context, app core.App, cfg SlugConfig, record *core.Record) error {
	if inProgress, _ := record.GetRaw(slugifyInProgressKey).(bool); inProgress {
		return nil
	}

	if err := validateConfigForCollection(app, cfg, record.Collection()); err != nil {
		return err
	}

	values := make([]string, 0, len(cfg.InputFields))
	for _, fieldName := range cfg.InputFields {
		resolved, err := resolveInputValues(app, record, fieldName)
		if err != nil {
			return err
		}

		values = append(values, resolved...)
	}

	baseSlug := buildSlug(values, cfg.Length)

	var lastErr error
	for attempt := 0; attempt < maxUniqueSlugAttempts; attempt++ {
		finalSlug, err := ensureUniqueSlug(app, record, cfg, baseSlug)
		if err != nil {
			return err
		}

		err = updateOutputField(ctx, app, record, cfg.OutputField, finalSlug)
		if err == nil {
			return nil
		}

		if !isUniqueConstraintError(err) {
			return err
		}

		lastErr = err
	}

	if lastErr != nil {
		return fmt.Errorf("failed to save a unique slug for field %q after %d attempts: %w", cfg.OutputField, maxUniqueSlugAttempts, lastErr)
	}

	return nil
}

func resolveInputValues(app core.App, record *core.Record, inputField string) ([]string, error) {
	segments := strings.Split(inputField, ".")
	if len(segments) == 1 {
		field := record.Collection().Fields.GetByName(inputField)
		if field == nil {
			return nil, fmt.Errorf("input field %q not found in collection %q", inputField, record.Collection().Name)
		}

		return flattenValueStrings(record.Get(field.GetName())), nil
	}

	currentCollection := record.Collection()
	currentRecords := []*core.Record{record}

	for i, segment := range segments {
		field := currentCollection.Fields.GetByName(segment)
		if field == nil {
			return nil, fmt.Errorf("input field %q not found in collection %q", segment, currentCollection.Name)
		}

		if i == len(segments)-1 {
			values := make([]string, 0, len(currentRecords))
			for _, currentRecord := range currentRecords {
				values = append(values, flattenValueStrings(currentRecord.Get(field.GetName()))...)
			}

			return values, nil
		}

		relationField, ok := field.(*core.RelationField)
		if !ok {
			return nil, fmt.Errorf("input field path %q in collection %q must use relation fields before the final segment", inputField, record.Collection().Name)
		}

		nextRecords, nextCollection, err := loadRelatedRecords(app, currentRecords, relationField)
		if err != nil {
			return nil, err
		}

		currentRecords = nextRecords
		currentCollection = nextCollection
	}

	return nil, nil
}

func loadRelatedRecords(app core.App, records []*core.Record, field *core.RelationField) ([]*core.Record, *core.Collection, error) {
	nextCollection, err := app.FindCachedCollectionByNameOrId(field.CollectionId)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load relation collection %q for field %q: %w", field.CollectionId, field.Name, err)
	}

	orderedIDs := make([]string, 0, len(records))
	seenIDs := make(map[string]struct{})
	uniqueIDs := make([]string, 0, len(records))

	for _, record := range records {
		for _, id := range record.GetStringSlice(field.Name) {
			orderedIDs = append(orderedIDs, id)
			if _, ok := seenIDs[id]; ok {
				continue
			}

			seenIDs[id] = struct{}{}
			uniqueIDs = append(uniqueIDs, id)
		}
	}

	if len(uniqueIDs) == 0 {
		return nil, nextCollection, nil
	}

	relatedRecords, err := app.FindRecordsByIds(nextCollection.Id, uniqueIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load related records for field %q: %w", field.Name, err)
	}

	recordIndex := make(map[string]*core.Record, len(relatedRecords))
	for _, relatedRecord := range relatedRecords {
		recordIndex[relatedRecord.Id] = relatedRecord
	}

	orderedRecords := make([]*core.Record, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		if relatedRecord := recordIndex[id]; relatedRecord != nil {
			orderedRecords = append(orderedRecords, relatedRecord)
		}
	}

	return orderedRecords, nextCollection, nil
}

func flattenValueStrings(value any) []string {
	if value == nil {
		return nil
	}

	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}

		return []string{v}
	case fmt.Stringer:
		return flattenValueStrings(v.String())
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		values := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			values = append(values, flattenValueStrings(rv.Index(i).Interface())...)
		}

		return values
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return []string{fmt.Sprint(value)}
	default:
		return nil
	}
}

func buildSlug(values []string, length int) string {
	combined := strings.Join(values, " ")
	combined = removeAccents(combined)
	combined = strings.ToLower(combined)
	combined = nonAlphaNumericPattern.ReplaceAllString(combined, "-")
	combined = duplicateHyphenPattern.ReplaceAllString(combined, "-")
	combined = strings.Trim(combined, "-")

	return truncateSlug(combined, length)
}

func removeAccents(value string) string {
	transformer := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, err := transform.String(transformer, value)
	if err != nil {
		return value
	}

	return result
}

func truncateSlug(slug string, length int) string {
	if length <= 0 {
		return ""
	}

	if len(slug) <= length {
		return strings.Trim(slug, "-")
	}

	return strings.Trim(slug[:length], "-")
}

func ensureUniqueSlug(app core.App, record *core.Record, cfg SlugConfig, baseSlug string) (string, error) {
	if baseSlug == "" {
		return "", nil
	}

	candidate := truncateSlug(baseSlug, cfg.Length)
	exists, err := slugExists(app, cfg.CollectionName, cfg.OutputField, record.Id, candidate)
	if err != nil {
		return "", err
	}

	if !exists {
		return candidate, nil
	}

	for i := 1; ; i++ {
		suffix := "-" + strconv.Itoa(i)
		stemLimit := cfg.Length - len(suffix)

		var next string
		if stemLimit > 0 {
			next = truncateSlug(baseSlug, stemLimit) + suffix
		} else {
			next = truncateSlug(strconv.Itoa(i), cfg.Length)
		}

		next = strings.Trim(next, "-")
		if next == "" {
			continue
		}

		exists, err = slugExists(app, cfg.CollectionName, cfg.OutputField, record.Id, next)
		if err != nil {
			return "", err
		}

		if !exists {
			return next, nil
		}
	}
}

func slugExists(app core.App, collectionName string, outputField string, recordID string, slug string) (bool, error) {
	records, err := app.FindRecordsByFilter(
		collectionName,
		fmt.Sprintf(`%s = {:slug} && id != {:id}`, outputField),
		"",
		1,
		0,
		map[string]any{
			"slug": slug,
			"id":   recordID,
		},
	)
	if err != nil {
		return false, err
	}

	return len(records) > 0, nil
}

func updateOutputField(ctx context.Context, app core.App, record *core.Record, outputField string, slug string) error {
	if record.GetString(outputField) == slug {
		return nil
	}

	record.SetRaw(slugifyInProgressKey, true)
	defer record.SetRaw(slugifyInProgressKey, nil)

	record.Set(outputField, slug)
	record.Set("updated", types.NowDateTime())

	return app.SaveNoValidateWithContext(ctx, record)
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

func (p *Plugin) beginRecalculation(recordID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.recalcInProgress == nil {
		p.recalcInProgress = make(map[string]struct{})
	}
	if _, ok := p.recalcInProgress[recordID]; ok {
		return false
	}

	p.recalcInProgress[recordID] = struct{}{}
	return true
}

func (p *Plugin) endRecalculation(recordID string) {
	p.mu.Lock()
	delete(p.recalcInProgress, recordID)
	p.mu.Unlock()
}

func (p *Plugin) runRecalculation(app core.App, cfg SlugConfig) {
	defer p.endRecalculation(cfg.Id)
	var recalcErr error
	if valid := validateConfigs(app, p.Name(), []SlugConfig{cfg}); len(valid) == 1 {
		recalcErr = recalculateCollection(app, valid[0])
	} else {
		recalcErr = errors.New("slug rule is no longer valid")
	}
	if recalcErr != nil {
		app.Logger().Error("slugify recalculation failed", slog.String("configId", cfg.Id), slog.Any("error", recalcErr))
	}

	record, err := app.FindRecordById(configsCollectionName, cfg.Id)
	if err != nil {
		return
	}
	record.Set("recalculating", false)
	if recalcErr == nil {
		record.Set(recalculationErrorField, "")
	} else {
		record.Set(recalculationErrorField, recalcErr.Error())
	}
	if err := app.SaveNoValidate(record); err != nil {
		app.Logger().Error("slugify failed to clear recalculation status", slog.String("configId", cfg.Id), slog.Any("error", err))
	}
}

func recalculateCollection(app core.App, cfg SlugConfig) error {
	lastID := ""

	for {
		filter := ""
		var params []dbx.Params
		if lastID != "" {
			filter = "id > {:lastId}"
			params = append(params, dbx.Params{"lastId": lastID})
		}

		records, err := app.FindRecordsByFilter(cfg.CollectionName, filter, "id", recalculateBatchSize, 0, params...)
		if err != nil {
			return fmt.Errorf("failed to load records for recalculation in collection %q: %w", cfg.CollectionName, err)
		}

		if len(records) == 0 {
			return nil
		}

		for _, record := range records {
			if err := processRecord(context.Background(), app, cfg, record); err != nil {
				return fmt.Errorf("failed to recalculate slug for record %q in collection %q: %w", record.Id, cfg.CollectionName, err)
			}
		}

		lastID = records[len(records)-1].Id
	}
}
