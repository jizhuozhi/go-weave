# Contributing

Thanks for looking. This is a low-level project — it forges runtime data
structures through `unsafe` and `//go:linkname` — so a few conventions matter
more here than in a typical library.

## Before you start

Three rules shape every design decision in this repo:

1. **No patching, no hooking.** The package never rewrites an existing
   function's machine code and never hooks a runtime function. It forges its own
   structures and uses documented (or fact-stable) mechanisms. A change that
   patches the runtime is out of scope.
2. **Fail loudly, never silently.** Every forged offset is validated at init
   against a real, runtime-built itab. An unsupported Go version must panic with
   a clear message rather than corrupt memory.
3. **`-race` stays clean.** From `dispatch` down, everything — interceptors,
   reflect, user code — is fully race-instrumented. Only the pure-register
   trampoline is excluded, and it must stay free of memory accesses for that to
   remain sound.

## Development setup

Go 1.18 or newer. No codegen step, no generated files — clone and build.

```sh
git clone git@github.com:jizhuozhi/go-weave.git
cd go-weave
go build ./...
```

## Running the tests

The full local check, matching what CI runs:

```sh
gofmt -l .                           # must print nothing
go vet -unsafeptr=false ./...        # unsafeptr is off by design
go test ./...
go test -race ./...
GODEBUG=clobberfree=1 go test -count=3 ./...   # GC safety stress
```

`GODEBUG=clobberfree=1` is not optional for changes that touch the register
plumbing, the trampolines, or the argument pointer maps — it is what catches a
pointer that survives only in an unscanned register.

Run the examples too; they exercise paths the unit tests do not:

```sh
go run ./examples/spi
(cd examples/dao && go run .)
(cd examples/rpc && go run .)
```

### Architecture notes

The test suite runs natively on arm64 and on amd64 (via Rosetta on Apple
Silicon). Both dispatch paths and both `redial` helpers are architecture-specific,
so a change to one usually needs a matching change to the other.

## Adding support for a new Go version

This is the most common maintenance task, and it has a fixed shape.

The forged `moduledata` layout is version-specific. Each segment lives in its own
file, guarded by a build tag:

| File | Covers | What changed |
| --- | --- | --- |
| `moduledata_go118.go` | 1.18, 1.19 | baseline |
| `moduledata_go120.go` | 1.20 | `covctrs` added |
| `moduledata_go121.go` | 1.21 | `inittasks` added |
| `moduledata_go123.go` | 1.23 | `bad` moved |
| `moduledata_go126.go` | 1.26 | `epclntab` added |
| `moduledata_go127.go` | 1.27+ | `typedesclen`, `itaboffset`/`itabsize`; `typelinks`/`itablinks` removed |

`rfunc_go1*.go` splits the `_func` mirror for the same reason (`startLine`
appeared in 1.20).

To add a version:

1. Diff `runtime/symtab.go`'s `moduledata` against the previous release and
   create or extend the matching `moduledata_go1XX.go`.
2. Update the expected-offset constants at the bottom of the file
   (`mdTextOff`, `mdPctabOff`, `mdGofuncOff`, `mdNextOff`). These are what
   `TestJITFindfunc` checks against the live runtime — do not guess them, read
   them off a real build.
3. Add the version to the `versions` matrix in
   [`.github/workflows/ci.yml`](.github/workflows/ci.yml). Every minor version
   from the floor to the latest is covered on linux/amd64, linux/arm64 and
   darwin/arm64.
4. Run the full local check above with that toolchain.

If the init-time self-check fires on a new Go version, that is the mechanism
working as intended — it means an offset moved, and the fix is a new segment,
not a relaxed check.

## Code conventions

- **Comments explain why, not what.** The interesting content in this codebase
  is the reasoning behind a layout or an ordering. Keep it.
- **No decorative separators.** Plain `/* ... */` or `//` comments.
- **`unsafe.Pointer` vs `uintptr` is load-bearing.** If you are changing a
  conversion between them, check whether the value must stay visible to the
  collector, and say so in a comment.
- **Ordering matters in the dispatcher.** The pointer mirror must be filled
  before the first allocation. Do not reorder that.
- Keep `gofmt` clean and `staticcheck` quiet (CI runs staticcheck 2025.1.1).

## Documentation

The whitepaper in [`docs/`](docs/) exists in English and Chinese and CI checks
that the two stay in parity, along with link integrity:

```sh
python3 scripts/check-docs.py
```

If you add or rename a chapter, update both language trees. Markdown is linted
with markdownlint-cli2 over `README.md`, `docs/**/*.md` and `mockito/**/*.md`.

## Reporting bugs

A useful report for this project includes:

- Go version, `GOARCH`, `GOOS`
- whether the failure happens under `-race` and/or `GODEBUG=clobberfree=1`
- the smallest interface + interceptor pair that reproduces it

If you are seeing a panic from the init-time self-check, paste the full message
— it names the offset and the expected value, which usually identifies the
missing version segment immediately.

## Security

This package intentionally reads and writes memory the Go runtime owns. If you
find a case where it can be made to corrupt memory rather than fail loudly,
that is a security issue — please report it privately rather than opening a
public issue.
