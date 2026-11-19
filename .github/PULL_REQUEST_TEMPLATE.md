## What this changes

<!-- One paragraph. What was wrong, what does it do now. -->

## Why

<!-- The failure or the requirement that forced this. Link the issue. -->

## Measurements

<!-- Required for anything on the packet path or the config path.
     Before / after, same method: loss, FIB/RIB counts, neighbour uptime, pids.
     Write "n/a" only for docs/typos. -->

| | Before | After |
| --- | --- | --- |
| | | |

## Interruption risk

<!-- Does this drop traffic that is already flowing, or restart a daemon?
     If yes: say what, and for how long you measured it. -->

## Checklist

- [ ] One topic, no unrelated refactors
- [ ] `go build ./... && go vet ./... && go test ./...` passes (and `-race` if shared state is touched)
- [ ] New behaviour has a test that fails without this change
- [ ] Console strings: English entry added alongside the Chinese one
- [ ] Commits are signed off (`git commit -s`, DCO)
