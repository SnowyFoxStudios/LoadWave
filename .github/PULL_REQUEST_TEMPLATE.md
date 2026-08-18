## What this changes

<!-- What the change does, and why. Link the issue it closes. -->

## How it was verified

<!--
Which tests cover it, and anything you checked by hand. For a change to load
generation or metric aggregation, say what run you did and what the numbers
looked like — correctness there is hard to see from the diff alone.
-->

## Checklist

- [ ] `make check` passes
- [ ] Tests cover the new behaviour, including the failure path
- [ ] Comments explain *why* where the reason is not obvious from the code
- [ ] Documentation updated, if behaviour or configuration changed
- [ ] `make generate` run and committed, if any `.proto` file changed
