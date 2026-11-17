# 0025: A typed keep count with a named unlimited value

Status: accepted
Date: 2026-11-17

## Context

A counted keep rule, `--keep-last` to `--keep-yearly`, keeps N snapshots or the newest snapshot of
each of N periods. There was no way to say "of every period": a yearly archive that should never
lose a year needed a number picked large enough. The counts were plain `int` fields of
`ForgetPolicy`, a negative one kept nothing, the same as zero, and nothing said so.

restic, whose keep rules ark follows, takes the word `unlimited` for these six flags and refuses a
negative number with a hint at the word. Inside it stores -1.

## Decision

The six counted fields of `ForgetPolicy` are of type `KeepCount`, an integer type, and the constant
`Unlimited` is the one count without a limit: `KeepLast: Unlimited` keeps every snapshot, a period
rule keeps the newest snapshot of every period. `Repository.Forget` refuses any other negative
count with `ErrInvalidPolicy` before it reads anything. An unlimited count is a rule, so such a
policy is not an empty one.

The text form is a whole number of zero or more, or the word `unlimited`. `ParseKeepCount` reads
it, and `KeepCount` reads and writes it as text, so the CLI flags and the keep keys of a profile
share one parser. `ark forget --keep-daily -1` is a usage error that names the word, and so is a
negative count in a profile.

- Not a bare -1 in the library, because a count computed wrong then keeps everything without a
  trace in the code that wrote it; `Unlimited` has to be named. The constant is -1 underneath, as
  in restic, so plain numbers still convert.
- Not -1 on the command line, as restic refuses it too: `--keep-daily -1` reads as a typo of 1,
  and a typo should not keep everything.
- Not a separate boolean per rule, because six more fields and flags would say what one value of
  the count says.

## Consequences

A policy with a negative count other than `Unlimited`, which kept nothing before, now fails. Code
that sets the fields from untyped constants compiles unchanged; code that assigns an `int` variable
converts it with `ark.KeepCount(n)`.

A profile takes a keep count as a quoted string too, `keep-daily: "7"`, since the key now reads
text; a fraction is still refused.

`TestForgetTakesUnlimitedAndRefusesOtherNegativeCounts` pins the unlimited rule and the refused
counts through `Repository.Forget`, `TestParseKeepCount` the text form and
`TestForgetTakesUnlimitedCounts` the flag, the profile key, `ark profile show` and both usage
errors.
