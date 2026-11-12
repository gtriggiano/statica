---
name: change-config-field
description: Add, change, rename or remove a field of the statica YAML configuration, with its default, its validation, its tests and its entry in the configuration reference that a Go test keeps in sync with the code. Use when a task adds an option to config.yaml or touches internal/config.
---

# Change a configuration field

The configuration file is a public interface. Every field has a default or a stated reason for having none, a validation rule, and a heading in `docs/reference/configuration.md` that `TestDocsConfigReference` compares with the code.

## Decide first

- **New field:** a `feat(config)` change.
- **Rename or removal:** a breaking change: `feat(config)!:` and a `BREAKING CHANGE: ` footer with the migration (see the `prepare-pr` skill).
- **New meaning for an existing key:** not allowed. Add a key with a new name and remove the old one, so that statica rejects an old file instead of reading it differently.
- **A new resource that clients can consume:** it needs a configured ceiling (see the design rules in `AGENTS.md`).

## Steps

1. **Declare the field** in its struct in `internal/config/types.go`, with a `yaml:"snake_case"` tag; the fields of a redirect rule are on `redirect.Rule` in `internal/redirect/redirect.go`. Use `time.Duration` for a duration, `ByteSize` for a size, and `int`, `bool`, `string`, or a slice or map of them. An optional block is a pointer to a struct, like `*TLS` or `*CORS`.
   Parsing is generic: strict keys, `null` rejection, `${NAME}` substitution and the redaction of substituted values already apply. Do not add per-field parsing.
2. **Set the default.** For a field of an always-present block, set it in `defaults()` in `types.go`. For a field inside an optional block, set it in `applyOptionalDefaults()` in `parse.go`, guarded by `c.present("<dotted.path>")`.
   A default copied from another field, as `admin.origin_health.key` is from `index`, also copies that field's `c.tainted` flag, so that `config print` redacts a substituted value.
3. **Validate** in `validate()` in `internal/config/validate.go`, or in the helper for its block (`validateServer`, `validateCache`, …). Report a problem with `c.err("<dotted.path>", "<problem>")`. The message names the path and the rule and never repeats the configured value.
4. **Use the field** in the package that owns the behavior, wired from `internal/app/app.go`, which builds every component from the validated configuration. The value never changes after startup.
5. **Test** parsing, the default and each validation error in `internal/config/config_test.go`, and the behavior in the owning package.
6. **Document the field** in `docs/reference/configuration.md`, under its block and at the heading level of its siblings:

   ```markdown
   ### `cache.max_entries`

   **Type:** integer\
   **Default:** `10000`\
   **Allowed:** positive

   Maximum number of entries, counted like `max_bytes`.
   ```

   - The heading is exactly the full path in backticks; a list item adds `[]` and a map entry `.<name>` (`headers.set.<name>`).
   - The type is the one the test derives from the Go type: `string`, `integer`, `boolean`, `duration`, `byte size`, `mapping` for a struct, `list of strings`, `list of mappings`, `mapping of strings`, `mapping of mappings`.
   - The default is a YAML value in backticks or one of four words: `required`; `unset — ` and what statica does then; ``inherits `<field>` `` for a rule field that falls back to a global one; ``derived from `<field>` `` for a default copied from another field. If omitting the field behaves like `0`, `false` or `""`, that value is its default: document it.
   - The `**Allowed:**` line states what validation accepts, when it is narrower than the type; every integer, duration and byte size has one. Start it, where it applies, with a form that the test checks: a list of values (`` `off` or `precompressed` ``), a range (`` `1` to `3` ``), `positive`, `zero or positive` or `nonempty`. Put any other constraint after `; `, and keep the line short: details go in the text below.
   - Add the field's row to the table at the top of its section: the field linked to its heading, then its type, its default without the explanation, and its allowed line.
7. **Update the other places that list defaults** in `docs/reference/configuration.md`: the "Defaults" section names the defaults that apply only when their block is present, and the "Environment variables" section names the defaults derived from another field.
8. **Let the test reach the field.** A new optional block, list or map needs an instance in `referenceBase` in `internal/config/docs_reference_test.go`. A default copied from another field goes in `derivedDefaults` in the same file.
9. **Update the JSON Schema.** Add a note for the field to `jsonSchemaNotes` in `internal/config/json_schema_test.go`: a one-sentence description, and the allowed values, bounds or syntax that hold whatever the other fields say. A field of a new optional block or list also needs that block in `jsonSchemaOptionalBlocks`. Then regenerate `docs/public/config.schema.json`:

   ```sh
   go test ./internal/config -run TestJSONSchema -update
   ```

   Add a case to `TestJSONSchemaRejectsInvalidConfigurations` for a new kind of rule. Never add a rule that rejects a file `statica config validate` accepts.
10. **Link, do not repeat.** A guide page that mentions the field links to its reference heading, such as `../reference/configuration.md#cache-max-entries`. Update a file in `docs/examples/` only when the complete example should show the field, and keep every `yaml [config.yaml]` snippet valid.
11. **Breaking change only:** update every page, snippet and example that uses the old key.

## Check

```sh
go test -race -count=1 ./internal/config -run 'TestDocsConfigReference|TestDocsConfigSnippets|TestRepositoryExamples|TestJSONSchema'
make check
make docs-build
make docs-check
```

`TestDocsConfigReference` fails on a missing or unknown field heading and on a type or default that differs from the code, and `TestJSONSchema` on a field without a note or a schema file that was not regenerated. Fix the page or the code; add a field to the lists of step 8, but never loosen a check to make it pass.
