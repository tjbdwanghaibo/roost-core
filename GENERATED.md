# Generated — do not edit here

This tree is the output of `roost project new planet -module example.com/planet -mods configdata,mongo,nats,dataengine,nest -template game-demo`
from roost-core 49f20ee (source head).
It exists to be cloned and read. Changes belong in roost-core/demo (the templates) or roost-core/codegen/internal/roost (the scaffold);
the next publish overwrites this branch.

To build it yourself, add a go.work pointing at a roost-core checkout: the wiring HEAD emits may need its main branch.

## generated-github/

A generated project ships its own CI under `.github/workflows/`. It is published here as
`generated-github/workflows/` because a GitHub Actions token is not allowed to push workflow
files into a branch — the refusal that kept this pipeline from ever publishing (RR-20260921-02).
Rename the directory back after cloning; the templates behind it are in roost-core/demo.
