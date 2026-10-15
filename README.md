# knowledge

A command-line tool written in Go that indexes local documents as embeddings
and searches them by semantic similarity: a local RAG index over your own
files. Everything stays on your machine except the calls to the embeddings
endpoint you configure.

## Requirements

- Go 1.25 or newer
- An OpenAI-compatible embeddings endpoint. The easiest is
  [Ollama](https://ollama.com) listening on `http://localhost:11434`:

  ```sh
  ollama pull bge-m3
  ```

  The defaults already point to `http://localhost:11434/v1` with the model
  `bge-m3`, so no configuration is needed. Any other OpenAI-compatible
  endpoint works too: set `KNOWLEDGE_API_BASE` (and `KNOWLEDGE_API_KEY` if
  that endpoint requires a credential).

## Install

With Go installed:

```sh
go install github.com/fredyk/knowledge/cmd/knowledge@latest
```

From a clone:

```sh
go install ./cmd/knowledge
```

or build a local binary:

```sh
go build -o knowledge ./cmd/knowledge
```

## Usage

### Index documents

```sh
knowledge regenerate [paths...]
```

Indexes the given paths (by default `~/knowledge`) into the local index. It
uses an incremental cache keyed by `(path, mtime, size)`, so unchanged files
are not re-embedded. Use `--force` to wipe the index and re-embed everything
from scratch.

```sh
knowledge regenerate
knowledge regenerate ~/docs ~/notes
knowledge regenerate ~/docs --force
```

### Search

```sh
knowledge search "<query>" [-k N] [--json]
```

Returns the most similar chunks with their similarity score.

```sh
knowledge search "deployment checklist"
knowledge search -k 3 "connection refused"
knowledge search --json "meeting notes" | jq
```

### Fetch official gazettes

```sh
knowledge fetch [--since YYYY-MM-DD] [--threshold 0.55] [--dry-run] [--boe-only|--dog-only]
```

Downloads new dispositions published in the BOE and the DOG (Spanish official
gazettes) that pass the relevance filter and writes them into `~/knowledge/`.
Candidates are ranked by similarity to your own seed queries: write the
topics you follow, in Spanish, one per line, in
`~/.config/knowledge/relevance-queries.txt`.

```sh
knowledge fetch --dry-run
knowledge fetch --since 2026-01-01
knowledge fetch --boe-only --threshold 0.6
```

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `KNOWLEDGE_API_BASE` | `http://localhost:11434/v1` | OpenAI-compatible embeddings endpoint. |
| `KNOWLEDGE_MODEL` | `bge-m3` | Embeddings model name. |
| `KNOWLEDGE_API_KEY` | unset | Optional. Sent as `Authorization: Bearer` only if set. |
| `KNOWLEDGE_MAX_REQ_PER_MIN` | unset | Unset: no limit. `0` disables it. `N` > 0 allows at most `N` requests per minute. |
| `KNOWLEDGE_DB` | `~/.local/share/knowledge-cli/index.db` | Path of the SQLite index database. |
| `KNOWLEDGE_CHUNK_SIZE` | `1200` | Chunk size in characters. |
| `KNOWLEDGE_CHUNK_OVERLAP` | `200` | Overlap between consecutive chunks, in characters. |
| `KNOWLEDGE_WORKERS` | `4` | Parallel workers used while indexing. |
| `KNOWLEDGE_API_BATCH_SIZE` | `32` | Texts per embeddings request. |
| `KNOWLEDGE_MAX_FILE_SIZE` | `10485760` | Maximum indexed file size, in bytes. |
| `KNOWLEDGE_RELEVANCE_QUERIES` | `~/.config/knowledge/relevance-queries.txt` | Seed queries for `fetch` ranking, one per line (`#` for comments). Without the file, a few generic examples are used. |
| `KNOWLEDGE_DEBUG` | unset | Set to `1` to enable debug logs. |

## Supported formats

`.txt`, `.md`, `.adoc`, `.rst`, `.org`, `.json`, `.yaml`, `.yml`, `.csv`,
`.tsv`, `.eml`, `.html`/`.htm`, `.docx`, `.xlsx`, and `.pdf`. PDF support
requires `pdftotext` (package `poppler-utils`) in the `PATH`; without it,
PDF files are skipped.

## License

MIT. See [LICENSE](LICENSE).
