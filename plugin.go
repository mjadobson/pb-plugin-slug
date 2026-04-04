package slugify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/dbutils"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/pocketbuilds/xpb"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var version = "dev"

const slugifyInProgressKey = "@slugify_in_progress"
const maxUniqueSlugAttempts = 32
const recalculateBatchSize = 100
const pluginsCollectionName = "_plugins"
const pluginNameField = "plugin_name"
const pluginConfigField = "config"
const pluginEnabledField = "enabled"

var nonAlphaNumericPattern = regexp.MustCompile(`[^a-zA-Z0-9]+`)
var duplicateHyphenPattern = regexp.MustCompile(`-+`)

type SlugConfig struct {
	CollectionName string   `json:"collection_name"`
	InputFields    []string `json:"input_fields"`
	OutputField    string   `json:"output_field"`
	Length         int      `json:"length"`
	Recalculate    bool     `json:"recalculate,omitempty"`
	Recalculating  bool     `json:"recalculating,omitempty"`
}

type Plugin struct {
	mu               sync.RWMutex
	activeConfigs    []SlugConfig
	recalcInProgress bool
}

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

func (p *Plugin) Init(app core.App) error {
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

	app.OnRecordAfterCreateSuccess(pluginsCollectionName).BindFunc(reloadPluginsHook)
	app.OnRecordAfterUpdateSuccess(pluginsCollectionName).BindFunc(reloadPluginsHook)
	app.OnRecordAfterDeleteSuccess(pluginsCollectionName).BindFunc(reloadPluginsHook)

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
	if err := ensurePluginsCollection(app); err != nil {
		return err
	}

	if err := p.reloadConfigs(app); err != nil {
		return err
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

func ensurePluginsCollection(app core.App) error {
	collection, err := app.FindCollectionByNameOrId(pluginsCollectionName)
	if err != nil {
		if !errorsIsNotFound(err) {
			return err
		}

		collection = core.NewBaseCollection(pluginsCollectionName)
	}

	if collection.Fields.GetByName(pluginNameField) == nil {
		collection.Fields.Add(&core.TextField{Name: pluginNameField, Required: true})
	}

	if collection.Fields.GetByName(pluginConfigField) == nil {
		collection.Fields.Add(&core.JSONField{Name: pluginConfigField})
	}

	if collection.Fields.GetByName(pluginEnabledField) == nil {
		collection.Fields.Add(&core.BoolField{Name: pluginEnabledField})
	}

	if _, ok := dbutils.FindSingleColumnUniqueIndex(collection.Indexes, pluginNameField); !ok {
		collection.AddIndex("idx__plugins_plugin_name_unique", true, "`plugin_name`", "")
	}

	return app.Save(collection)
}

func (p *Plugin) reloadConfigs(app core.App) error {
	record, enabled, err := p.loadConfigRecord(app)
	if err != nil {
		p.mu.Lock()
		p.activeConfigs = nil
		p.mu.Unlock()

		return err
	}

	if !enabled {
		p.mu.Lock()
		p.activeConfigs = nil
		p.mu.Unlock()

		return nil
	}

	configs, err := decodeConfigs(record.GetRaw(pluginConfigField))
	if err != nil {
		p.mu.Lock()
		p.activeConfigs = nil
		p.mu.Unlock()

		return fmt.Errorf("%s: %w", p.Name(), err)
	}

	if hasRecalculateRequest(configs) && p.beginRecalculation() {
		defer p.endRecalculation()

		configs, err = p.runRecalculation(app, record, configs)
		if err != nil {
			p.mu.Lock()
			p.activeConfigs = nil
			p.mu.Unlock()

			return err
		}
	}

	p.mu.Lock()
	p.activeConfigs = slicesClone(validateConfigs(app, p.Name(), configs))
	p.mu.Unlock()

	return nil
}

func (p *Plugin) loadConfigRecord(app core.App) (*core.Record, bool, error) {
	record, err := app.FindFirstRecordByFilter(
		pluginsCollectionName,
		pluginNameField+" = {:pluginName}",
		map[string]any{"pluginName": p.Name()},
	)
	if err != nil {
		if errorsIsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, err
	}

	if !record.GetBool(pluginEnabledField) {
		return nil, false, nil
	}

	return record, true, nil
}

func decodeConfigs(rawValue any) ([]SlugConfig, error) {
	raw, ok := rawValue.(types.JSONRaw)
	if !ok {
		return nil, fmt.Errorf("config field is not json")
	}

	var configs []SlugConfig
	if err := json.Unmarshal(raw, &configs); err != nil {
		return nil, fmt.Errorf("invalid config json: %w", err)
	}

	return configs, nil
}

func (p *Plugin) handleRecordEvent(ctx context.Context, app core.App, record *core.Record) error {
	if record.Collection().Name == pluginsCollectionName {
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

func hasRecalculateRequest(configs []SlugConfig) bool {
	for _, cfg := range configs {
		if cfg.Recalculate {
			return true
		}
	}

	return false
}

func normalizeRecalculateFlags(configs []SlugConfig) []SlugConfig {
	normalized := slicesClone(configs)
	for i := range normalized {
		if normalized[i].Recalculate {
			normalized[i].Recalculate = false
			normalized[i].Recalculating = true
		}
	}

	return normalized
}

func clearRecalculatingFlags(configs []SlugConfig) []SlugConfig {
	cleared := slicesClone(configs)
	for i := range cleared {
		cleared[i].Recalculate = false
		cleared[i].Recalculating = false
	}

	return cleared
}

func (p *Plugin) beginRecalculation() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.recalcInProgress {
		return false
	}

	p.recalcInProgress = true
	return true
}

func (p *Plugin) endRecalculation() {
	p.mu.Lock()
	p.recalcInProgress = false
	p.mu.Unlock()
}

func (p *Plugin) runRecalculation(app core.App, record *core.Record, configs []SlugConfig) (finalConfigs []SlugConfig, err error) {
	runningConfigs := normalizeRecalculateFlags(configs)
	if err := updatePluginConfigRecord(app, record, runningConfigs); err != nil {
		return nil, err
	}

	defer func() {
		clearedConfigs := clearRecalculatingFlags(runningConfigs)
		cleanupErr := updatePluginConfigRecord(app, record, clearedConfigs)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("failed to clear recalculation flags: %w", cleanupErr)
			if err != nil {
				err = errors.Join(err, cleanupErr)
			} else {
				err = cleanupErr
			}
			return
		}

		if err == nil {
			finalConfigs = clearedConfigs
		}
	}()

	for _, cfg := range validateConfigs(app, p.Name(), runningConfigs) {
		if !cfg.Recalculating {
			continue
		}

		if err := recalculateCollection(app, cfg); err != nil {
			return nil, err
		}
	}

	return finalConfigs, nil
}

func updatePluginConfigRecord(app core.App, record *core.Record, configs []SlugConfig) error {
	raw, err := json.Marshal(configs)
	if err != nil {
		return err
	}

	record.SetRaw(pluginConfigField, types.JSONRaw(raw))
	record.Set("updated", types.NowDateTime())

	return app.SaveNoValidate(record)
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
