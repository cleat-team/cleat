# ts-workflow

The smallest TypeScript workflow that does something durable: two service calls, the second fed by the first.

**TypeScript is tier 2: in development, not supported** (`tiers.yaml`, cleat#3825). Read
[the getting-started page](../../docs/tutorials/typescript-quick-start.md) for what that means before you depend on it.

Build it (from the repository root, after `packages/cleat-ts/bootstrap.sh`):

```
cleat build --target ts -o /tmp/out examples/ts-workflow/workflow.ts:greet
```

`examples/ts-workflow/workflow.test.ts` runs exactly that command and then runs the module through the test harness, in
CI, on every pull request that touches this directory.
