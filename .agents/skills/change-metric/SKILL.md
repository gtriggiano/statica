---
name: change-metric
description: Add, change, rename or remove a statica Prometheus metric, with bounded labels, tests and the entry in the metrics reference that a Go test keeps in sync with the exported families. Use when a task touches internal/metrics or a statica_ metric name, type, label or bucket.
---

# Change a metric

Metric names, types and labels are a public interface: dashboards and alerts depend on them. `TestMetricsReference` checks `docs/reference/metrics.md` against the families the code exports, in both directions.

## Decide first

- **New family, new label value:** a `feat(metrics)` change.
- **Renamed or removed family, renamed or removed label, changed type or buckets:** a breaking change: `feat(metrics)!:` and a `BREAKING CHANGE: ` footer (see the `prepare-pr` skill).
- **Labels are bounded.** Every label value comes from a fixed set listed in the reference, except `host`, the HTTP `status` and the labels of `statica_build_info`. Paths, object keys, queries, IP addresses and request IDs never become label values: `TestNoRequestDataInLabels` guards this.

## Steps

1. **Find where the family lives** in `internal/metrics/`:
   - `New()` in `metrics.go` creates and registers every family except those of `cache.go` and `state.go`; `http.go` and `origin.go` record into them;
   - `cache.go` describes the memory cache families that are read at scrape time;
   - `state.go` describes the readiness and origin health families and `statica_log_events_dropped_total`.

   Request metrics come from the per-request record in `internal/telemetry`, which also feeds the access log: add a field to the record rather than a second measurement path.
2. **Define the family** next to those of the same area, with the `statica_` prefix, base units (seconds, bytes), `_total` at the end of a counter and `_seconds` at the end of a histogram. Follow the area's rule for when series appear: some families export every series from startup, others create a series on its first observation.
3. **Test** the values in `internal/metrics/metrics_test.go` or the matching `*_test.go`, through the public boundary when the metric comes from a request, as `contract_test.go` does. Then add the family to the lists that the guard tests keep:
   - the `want` list of `TestMetricNamesFollowConventions` in `metrics_test.go`;
   - for a family whose series appear on first observation, the same observation in `TestMetricNamesFollowConventions` and in `exportedFamilies` in `docs_reference_test.go`: both gather only the families that have a series;
   - for a family with labels whose every series exists from startup, `startupFamilies` in `docs_reference_test.go`, so that the test requires its section to name every label value.
4. **Document the family** in `docs/reference/metrics.md`:
   - one row in the summary table at the top, in the order of the sections: family linked to its heading, type, labels;
   - one heading named `` `statica_<name>` `` in its section, followed by `**Type:**` and `**Labels:**` lines (and `**Buckets:**` for a histogram), then what it counts and a table of every value of each fixed label;
   - a "When series exist" table, under "Labels and series", whose rows still describe the family.
5. **Guide.** When the metric changes what operators should watch, update `docs/guide/metrics.md`, linking to the reference instead of repeating it.

## Check

```sh
go test -race -count=1 ./internal/metrics
make check
make docs-build
make docs-check
```

`TestMetricsReference` fails when an exported family is not documented, when a documented family is not exported, and when the type, labels or buckets differ. Fix the page or the code; add a family to the lists above, but never loosen a check to make it pass.
