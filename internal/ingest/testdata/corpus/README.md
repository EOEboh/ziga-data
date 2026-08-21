# Ingestion fixture corpus

Realistic inbound mail, and what the pipeline must do with each message.

Every case is a **triple** sharing one base name:

| File | What it is | Who reads it |
|---|---|---|
| `NAME.eml` | The raw RFC-822 message | `worker/email-ingest` tests, and humans |
| `NAME.json` | The `ingest.Message` the worker produces from it | this package's tests |
| `NAME.want.json` | The expected `Outcome` (and, later, resolved origin) | this package's tests |

## Why the `.eml` exists if Go never parses it

The Go pipeline's input is the `.json` — reproducing the worker's MIME parsing
in Go would mean maintaining a second parser whose bugs differ from the real
one's, which is worse than useless.

The `.eml` earns its place twice over. It is the human-auditable provenance of
the `.json`: when a fixture asserts something surprising, the raw message shows
why. And `worker/email-ingest/test/parse.test.ts` reads **these same files** and
asserts its parser plus payload builder produce exactly the committed `.json`.

That makes the corpus a genuine cross-language contract rather than two
fixture sets that drift apart. A change to the worker's output shape fails in
the worker's own tests, rather than silently changing what gets filtered here.

## Adding a case

Drop in three files and the test picks them up — `corpus_test.go` walks this
directory and makes a subtest per base name.

Name the fixture for **what it proves**, not what it is: `noreply-sender`,
`gmail-manual-forward-thread-3`, `fake-confirmation-wrong-sender`. When a
subtest fails, its name should tell you which property broke.

To regenerate the `.want.json` files after a deliberate behaviour change:

```
go test ./internal/ingest/ -run TestCorpus -update
```

Read the diff before committing it. The point of a golden file is that an
unintended change is visible, and `-update` will happily bless a regression.
