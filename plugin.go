package slugify

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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

var nonAlphaNumericPattern = regexp.MustCompile(`[^a-zA-Z0-9]+`)
var duplicateHyphenPattern = regexp.MustCompile(`-+`)

type SlugConfig struct {
	CollectionName string   `json:"collection_name"`
	InputFields    []string `json:"input_fields"`
	OutputField    string   `json:"output_field"`
	Length         int      `json:"length"`
}

type Plugin struct {
	Configs []SlugConfig `json:"configs"`
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
	if err := p.validateConfigShapes(); err != nil {
		return err
	}

	if err := p.validateStartupConfigs(app); err != nil {
		return err
	}

	for _, cfg := range p.Configs {
		config := cfg

		app.OnRecordAfterCreateSuccess(config.CollectionName).BindFunc(func(e *core.RecordEvent) error {
			if err := processRecord(e.Context, e.App, config, e.Record); err != nil {
				e.App.Logger().Error(
					"slugify create hook failed",
					slog.String("collection", config.CollectionName),
					slog.String("recordId", e.Record.Id),
					slog.Any("error", err),
				)
			}

			return e.Next()
		})

		app.OnRecordAfterUpdateSuccess(config.CollectionName).BindFunc(func(e *core.RecordEvent) error {
			if err := processRecord(e.Context, e.App, config, e.Record); err != nil {
				e.App.Logger().Error(
					"slugify update hook failed",
					slog.String("collection", config.CollectionName),
					slog.String("recordId", e.Record.Id),
					slog.Any("error", err),
				)
			}

			return e.Next()
		})
	}

	return nil
}

func (p *Plugin) validateConfigShapes() error {
	if len(p.Configs) == 0 {
		return fmt.Errorf("%s: at least one config entry is required", p.Name())
	}

	seenTargets := make(map[string]int, len(p.Configs))

	for i, cfg := range p.Configs {
		if cfg.CollectionName == "" || cfg.OutputField == "" || cfg.Length <= 0 || len(cfg.InputFields) == 0 {
			return fmt.Errorf("%s: config %d must include collection_name, input_fields, output_field, and a positive length", p.Name(), i)
		}

		key := cfg.CollectionName + "\x00" + cfg.OutputField
		if firstIndex, ok := seenTargets[key]; ok {
			return fmt.Errorf("%s: config %d duplicates config %d for collection_name=%q and output_field=%q", p.Name(), i, firstIndex, cfg.CollectionName, cfg.OutputField)
		}

		seenTargets[key] = i
	}

	return nil
}

func (p *Plugin) validateStartupConfigs(app core.App) error {
	for i, cfg := range p.Configs {
		collection, err := app.FindCachedCollectionByNameOrId(cfg.CollectionName)
		if err != nil {
			app.Logger().Warn(
				"slugify config warning",
				slog.Int("configIndex", i),
				slog.String("collection", cfg.CollectionName),
				slog.Any("error", fmt.Errorf("collection %q not found yet: %w", cfg.CollectionName, err)),
			)
			continue
		}

		if err := validateConfigForCollection(cfg, collection); err != nil {
			return fmt.Errorf("%s: config %d invalid: %w", p.Name(), i, err)
		}
	}

	return nil
}

func validateConfigForCollection(cfg SlugConfig, collection *core.Collection) error {
	for _, fieldName := range cfg.InputFields {
		if fieldName == "" {
			return fmt.Errorf("input field names in collection %q cannot be empty", cfg.CollectionName)
		}

		if fieldName == cfg.OutputField {
			return fmt.Errorf("output field %q in collection %q cannot also be listed in input_fields", cfg.OutputField, cfg.CollectionName)
		}

		if collection.Fields.GetByName(fieldName) == nil {
			return fmt.Errorf("input field %q not found in collection %q", fieldName, cfg.CollectionName)
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

	if err := validateConfigForCollection(cfg, record.Collection()); err != nil {
		return err
	}

	values := make([]string, 0, len(cfg.InputFields))
	for _, fieldName := range cfg.InputFields {
		values = append(values, record.GetString(fieldName))
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
