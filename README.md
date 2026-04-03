# Slugify

Generate PocketBase slugs from one or more record fields.

The plugin combines configured input fields in order, normalizes the text to lowercase alphanumeric segments separated by hyphens, truncates the result to the configured length, and adds numeric suffixes when needed to keep slugs unique.

## Installation

Build PocketBase with the plugin:

```sh
xpb build --with github.com/mjadobson/pb-plugin-slug@latest
```

## Setup

On startup the plugin creates a shared `_plugins` collection if it doesn't already exist.

Create or update the `slugify` row in `_plugins` with:

- `plugin_name`: `slugify`
- `enabled`: `true` or `false`
- `config`: JSON config

Example `config` value:

```json
[
  {
    "collection_name": "posts",
    "input_fields": ["title", "subtitle"],
    "output_field": "slug",
    "length": 64
  },
  {
    "collection_name": "pages",
    "input_fields": ["title"],
    "output_field": "slug",
    "length": 32
  }
]
```

Then make sure each target collection has:

- the fields listed in `input_fields`
- a text or editor field matching `output_field`
- a single-column `UNIQUE` index on `output_field` (recommended with `WHERE output_field != ''`)

The plugin runs after successful record create and update operations.
It reloads its active config whenever any row in `_plugins` is created, updated, or deleted.
It also reloads when collections are created, updated, or deleted so deferred configs can become active after schema changes.
It does not backfill slugs for existing records; it only applies to records saved after the config is active.

## Unique Index Requirement

`output_field` must have its own single-column unique index.

This plugin checks for existing slugs before saving, but concurrent writes can still race unless the database also enforces uniqueness. The unique index is what guarantees two records cannot end up with the same slug.

For SQLite-backed PocketBase collections, a good pattern is:

```sql
CREATE UNIQUE INDEX `idx_posts_slug_unique` ON `posts` (`slug`) WHERE `slug` != ''
```

Replace `posts` and `slug` with your collection and field names.

## Plugin Config

### `[].collection_name`

The PocketBase collection name to watch.

### `[].input_fields`

The fields to combine, in order, before generating the slug.

### `[].output_field`

The text or editor field where the slug should be written.

### `[].length`

The maximum slug length.

## Behaviour

- Input field values are joined with spaces in config order.
- Accents are removed where possible.
- Non-alphanumeric characters become hyphens.
- Repeated separators are collapsed and trimmed.
- Slugs are lowercased and truncated to `length`.
- If the slug already exists in the same collection, `-1`, `-2`, and so on are appended while staying within `length`.
- If all configured inputs are blank, the output slug is cleared.
- The output field must have its own unique index so concurrent writes cannot create duplicate slugs.
- Invalid config entries are skipped individually so valid rules can continue to run.

## Development

```sh
go mod tidy
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build ./...
```
