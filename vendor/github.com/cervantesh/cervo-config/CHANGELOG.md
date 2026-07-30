# Changelog

## Unreleased

- Add `cervoclaw.NewCloudLoader` with optional Cloud Run/agentic config file
  and mounted secret directory sources.
- Add fail-fast `MustDecode` helpers for root loaders and `structenv`.
- Add sensitive metadata, redacted descriptions, and Markdown config docs.
- Add struct decoding and validation support for `[]int` and
  `map[string]string`.
- Add a zero-dependency Google Secret Manager source adapter example.

## v0.3.0 - 2026-05-25

- Add ordered configuration sources for env, flags, maps, JSON files, mounted
  secret directories, and remote adapters.
- Add struct decoding with `config`, `alias`, `default`, `required`, `desc`,
  and `sep` tags.
- Add metadata registration with `Register`, `Describe`, and `Validate`.
- Add custom parsers with `RegisterParser` and `Parse`.
- Add `encoding.TextUnmarshaler` support for custom struct field types.
- Add `structenv` subpackage for explicit struct-based decoding.
- Add release docs, examples, and CI.

## v0.2.0 - 2026-05-25

- Add reusable loaders and typed helpers.
- Add strict parsers.
- Add `cervoclaw` compatibility subpackage.

## v0.1.0 - 2026-05-25

- Initial environment helper package.
