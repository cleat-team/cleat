# Cleat Web UI

Svelte 5 single-page application for managing and monitoring Cleat durable workflows. Provides a visual dashboard for workflow lifecycle, DAG inspection, cost observability, schedule management, and dead-letter queue processing.

## Screenshots

> Screenshots TBD. Placeholder references for future documentation:
>
> - `screenshots/dashboard.png` -- Main dashboard with workflow status overview
> - `screenshots/workflow-detail.png` -- Single workflow instance detail view with event timeline
> - `screenshots/definitions.png` -- Workflow definitions browser with version metadata
> - `screenshots/dag-graph.png` -- DAG graph visualization showing task dependencies

---

## Architecture

| Layer              | Technology                        |
|--------------------|-----------------------------------|
| Framework          | Svelte 5 with runes (`$state`, `$derived`, `$effect`) |
| Language           | TypeScript 5.5+                   |
| Bundler            | Vite 6                            |
| Test runner        | Vitest 2 + jsdom                  |
| Testing library    | @testing-library/svelte 5         |
| CSS                | Plain CSS (no framework)          |

The UI is a **static single-page application**. It communicates with the Cleat worker via same-origin REST API calls to `/api/...` (see Authentication, below, for how those calls are authorized). There is no server-side rendering -- all data is fetched client-side and rendered in the browser.

### Project structure

```
web/
  index.html              # HTML entry point
  vite.config.ts          # Vite configuration (build output → worker web dist)
  vitest.config.ts        # Vitest configuration with jsdom environment
  tsconfig.json           # TypeScript configuration
  svelte.config.js        # Svelte 5 compiler configuration
  package.json            # Dependencies and scripts
  src/
    main.ts               # Application bootstrap (mounts App.svelte)
    App.svelte            # Root component with routing and layout
    app.css               # Global styles
    setup.ts              # Test setup (global mocks, matchers)
    lib/
      api.ts              # REST API client functions
      auth.ts             # API key storage (localStorage) and 401 notifications
      auth.test.ts        # Auth token storage tests
      types.ts            # TypeScript interfaces (WorkflowInstance, EventRecord, etc.)
      cost.ts             # Cost calculation helpers
      cost.test.ts        # Cost helper tests
      api.test.ts         # API client tests
      api.auth.test.ts    # Authorization header tests
    components/
      DAGGraph.svelte     # Directed-acyclic graph visualization
      SummaryCard.svelte  # Summary statistics card
      CostSummary.svelte  # Cost summary display
      CostPanel.svelte    # Detailed cost breakdown panel
      EventTimeline.svelte # Workflow event history timeline
      Sidebar.svelte      # Navigation sidebar
      ApiKeyGate.svelte   # Paste-your-API-key modal
      StatusBadge.svelte  # Workflow status indicator badge
      StatusBadge.test.ts # StatusBadge tests
      SummaryCard.test.ts # SummaryCard tests
    pages/
      Dashboard.svelte          # Main overview dashboard
      WorkflowList.svelte       # List all workflow instances
      WorkflowDetail.svelte     # Single workflow instance detail
      WorkflowCompare.svelte    # Side-by-side workflow comparison
      Definitions.svelte        # Workflow definition browser
      DeadLetters.svelte        # Dead-letter queue management
      ScheduleManagement.svelte # Cron schedule management
```

### REST API Layer

All API calls are defined in `src/lib/api.ts` and return typed promises. The API surface covers:

| Endpoint                       | Method | Purpose                        |
|--------------------------------|--------|--------------------------------|
| `/api/workflows`               | GET    | List workflow instances        |
| `/api/workflows/:id`           | GET    | Get single workflow instance   |
| `/api/workflows/:name/start`   | POST   | Start a new workflow           |
| `/api/workflows/:id/signal`    | POST   | Signal a running workflow      |
| `/api/workflows/:id/cancel`    | POST   | Cancel a workflow              |
| `/api/workflows/:id/history`   | GET    | Get workflow event history     |
| `/api/workflows/:id/dag`       | GET    | Get workflow DAG structure     |
| `/api/workflows/:id/query`     | GET    | Get queryable workflow state   |
| `/api/workflows/batch-history` | POST   | Get histories for comparison   |
| `/api/definitions`             | GET    | List workflow definitions      |
| `/api/schedules`               | GET    | List schedules                 |
| `/api/schedules`               | POST   | Create schedule                |
| `/api/schedules/:name`         | DELETE | Delete schedule                |
| `/api/schedules/:name/enable`  | POST   | Enable schedule                |
| `/api/schedules/:name/disable` | POST   | Disable schedule               |
| `/api/dead-letters`            | GET    | List dead-letter workflow runs |
| `/api/dead-letters/:id/reprocess` | POST | Reprocess a dead-letter     |
| `/api/dead-letters/:id/terminate` | POST | Terminate a dead-letter    |

---

## Quick Start

```bash
# Install dependencies
npm install

# Start development server with hot reload
npm run dev
```

The dev server runs at `http://localhost:5173` by default and proxies API requests to the running Cleat worker.

### Prerequisites

- Node.js 18+
- npm 9+

---

## Authentication

There is no `API_BASE_URL` or `AUTH_TOKEN` environment variable. The dashboard is a
static bundle embedded in the `cleat-worker` binary (see Build, below) -- it is not
rebuilt per deployment, so there is no build step where an env var could be baked in,
and the worker serves it from a plain `http.FileServer` with no template step where a
value could be injected at request time either.

The worker's supported configuration defaults to `--require-auth=true`
(`cmd/cleat-worker/config.go`), which rejects every request without a valid API key
except `/healthz` and `/metrics` (`auth/middleware.go`). So instead of an env var, the
dashboard asks for the key at runtime, in the browser:

- `src/lib/auth.ts` stores the key in `localStorage` under `cleat_api_token`, scoped to
  the origin the dashboard is served from.
- `src/lib/api.ts`'s single `fetchJSON` wrapper -- every API call in this file goes
  through it -- attaches it as `Authorization: Bearer <token>` on every request when one
  is stored, and does nothing extra when one is not (so an unauthenticated dev
  deployment, `--require-auth=false`, is unaffected).
- `src/components/ApiKeyGate.svelte` is the paste-your-key UI. It opens automatically
  the first time any request comes back `401` (`lib/auth.ts`'s `notifyUnauthorized`,
  wired up in `App.svelte`), and can also be opened proactively from the "API Key" link
  in the sidebar.

Get a key from the worker's own startup log (it auto-generates and prints one the first
time it boots against a database with no keys yet, `cmd/cleat-worker/main.go`), or mint
one with:

```bash
cleat-worker --db "$DATABASE_URL" --generate-api-key "<tenant-uuid>"
```

The token never leaves the browser except as the `Authorization` header sent to this
same worker; it is not sent to any other origin.

---

## Testing

The project uses **Vitest** with **jsdom** for component and unit tests.

```bash
# Run all tests once
npm test

# Run tests in watch mode
npm run test:watch
```

### Test files

Test files are co-located with source files using the `*.test.ts` naming convention:

- `src/components/StatusBadge.test.ts` -- StatusBadge component tests
- `src/components/SummaryCard.test.ts` -- SummaryCard component tests
- `src/lib/api.test.ts` -- API client unit tests
- `src/lib/api.auth.test.ts` -- Authorization header unit tests
- `src/lib/auth.test.ts` -- API key storage unit tests
- `src/lib/cost.test.ts` -- Cost calculation unit tests

### Test configuration

Vitest is configured in `vitest.config.ts`:

- Environment: `jsdom` (DOM simulation in Node.js)
- Global test helpers enabled
- Setup file: `src/setup.ts` (global mocks and matcher extensions)
- Svelte components rendered via `@testing-library/svelte`

---

## Build

```bash
npm run build
```

Produces a static build in `../cmd/cleat-worker/web/dist` (configured in `vite.config.ts`
-- re-derive with `grep outDir web/vite.config.ts`; that file's own comment explains why
this has drifted to the wrong `durable-worker` path more than once). The cleat-worker
binary embeds these static files and serves them at the root URL.

### Build output

```
cmd/cleat-worker/web/dist/
  index.html
  assets/
    index-*.js
    index-*.css
```

### Preview production build

```bash
npm run preview
```

---

## Component Overview

### DAGGraph.svelte

Visualizes a workflow's directed-acyclic graph showing task dependencies. Each task is rendered as a node with edges drawn between parent and child tasks. Handles empty states (no DAG available) gracefully.

### SummaryCard.svelte

Reusable card component for displaying a summary metric with a label and optional sub-value. Used across multiple pages for counts, rates, and status summaries.

### CostSummary.svelte

Aggregated cost display broken down by model and provider. Shows total tokens, cost per LLM call, and per-workflow cost summaries. Uses the `CostBreakdown` and `WorkflowCost` types from `types.ts`.

### CostPanel.svelte

Expanded cost detail panel showing per-call LLM cost with token usage breakdown. Renders a table of individual LLM call records with model, provider, token counts, and computed cost.

### EventTimeline.svelte

Chronological view of a workflow's event history. Each event step shows its type, service call details, duration, and status. Supports filtering by event type.

### Sidebar.svelte

Navigation sidebar with links to all major pages: Dashboard, Workflows, Definitions, Dead Letters, and Schedules. Highlights the currently active page.

### StatusBadge.svelte

Color-coded badge indicating workflow status. Maps status strings (running, completed, failed, cancelled, timed_out, suspended) to appropriate visual styles.

---

## Pages Overview

### Dashboard (Dashboard.svelte)

Main landing page showing:
- Aggregate workflow counts by status (running, completed, failed, etc.)
- Recent workflow activity
- Quick-action buttons for common operations

### WorkflowList (WorkflowList.svelte)

Lists all workflow instances with filtering by status. Displays workflow ID, definition name, version, status, timestamps, and assigned worker. Links to the detail view for each instance.

### WorkflowDetail (WorkflowDetail.svelte)

Single workflow instance view with:
- Full workflow metadata (ID, definition, version, status, input/output)
- Event timeline showing each durable call in the workflow's history
- DAG visualization of task dependencies
- Cost breakdown for LLM-instrumented workflows
- Action buttons for signalling, cancelling, and querying state

### WorkflowCompare (WorkflowCompare.svelte)

Side-by-side comparison of multiple workflow instances. Select workflows and view their event histories aligned by step index. Useful for debugging differences between successful and failed runs.

### Definitions (Definitions.svelte)

Browses registered workflow definitions. Shows for each definition: name, version, ABI version, minimum supported version, active instance count, and memory usage statistics (min/avg/max/p50/p90/p99). Indicates deprecated definitions.

### DeadLetters (DeadLetters.svelte)

Manages the dead-letter queue -- workflow instances that failed with non-retryable errors. Lists failed instances with their error details and provides actions to reprocess or terminate.

### ScheduleManagement (ScheduleManagement.svelte)

Manages cron-based workflow schedules. Displays all schedules with their cron expression, target workflow, enabled/disabled status, and next run time. Supports creating, editing, enabling, disabling, and deleting schedules.

---

## Development Notes

### Adding a new page

1. Create a `.svelte` file in `src/pages/`
2. Add a route in `App.svelte`
3. Add a nav link in `Sidebar.svelte`
4. Export any needed API functions in `src/lib/api.ts`

### Adding a new component

1. Create a `.svelte` file in `src/components/`
2. Define TypeScript interfaces in `src/lib/types.ts` if needed
3. Add a co-located test file with `*.test.ts` suffix
4. Import and use in the relevant page

### Cross-language deterministic cost calculation

Cost calculation logic lives in `src/lib/cost.ts` and mirrors the Go-side cost computation. Changes to pricing logic must be applied to both the Go backend and this TypeScript module to keep the UI and API costs in sync.

---

## Further Reading

- [Cleat project README](../README.md) -- Architecture overview, worker deployment, CLI reference
- [Cleat WASM ABI specification](../ABI.md) -- Full ABI contract for workflow authors
- [Worker configuration reference](../docs/reference/worker-config.md) -- Worker config options for API and web serving
