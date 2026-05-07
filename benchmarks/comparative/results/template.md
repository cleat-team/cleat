# Benchmark Comparison Results

**Generated**: {{DATE}}
**Run by**: {{USER}}

## System Information

- **Date**: {{DATE_ISO}}
- **Hostname**: {{HOSTNAME}}
- **CPU**: {{CPU_MODEL}}
- **Cores**: {{CORES}} ({{LOGICAL_CORES}} logical)
- **RAM**: {{RAM_TOTAL}}
- **Disk**: {{DISK_MODEL}} ({{DISK_TYPE}}, {{FILESYSTEM}})
- **Kernel**: {{KERNEL}}
- **Go**: {{GO_VERSION}}
- **Node**: {{NODE_VERSION}}
- **PostgreSQL**: {{PG_VERSION}} (shared_buffers={{PG_SHARED_BUFFERS}})

### Benchmark configuration

- Warm-up: {{WARMUP_DURATION}}
- Measurement window: {{MEASURE_DURATION}}
- CPU governor: {{CPU_GOVERNOR}}
- Turbo Boost: {{TURBO_BOOST_STATE}}
- Frameworks tested: {{FRAMEWORKS}}

---

## Comparison Table

| Workload              | Config         | Cleat (steps/s) | Temporal (steps/s) | DBOS (steps/s) | Cleat vs Temporal | Cleat vs DBOS |
|-----------------------|----------------|-----------------|--------------------|----------------|-------------------|---------------|
| Simple                | steps=10       | {{C_SIMP_10}}   | {{T_SIMP_10}}      | {{D_SIMP_10}}  | {{CT_SIMP_10}}    | {{CD_SIMP_10}}|
| Simple                | steps=100      | {{C_SIMP_100}}  | {{T_SIMP_100}}     | {{D_SIMP_100}} | {{CT_SIMP_100}}   | {{CD_SIMP_100}}|
| Simple                | steps=1000     | {{C_SIMP_1000}} | {{T_SIMP_1000}}    | {{D_SIMP_1000}}| {{CT_SIMP_1000}}  | {{CD_SIMP_1000}}|
| Fan-out               | children=10    | {{C_FAN_10}}    | {{T_FAN_10}}       | {{D_FAN_10}}   | {{CT_FAN_10}}     | {{CD_FAN_10}}  |
| Fan-out               | children=100   | {{C_FAN_100}}   | {{T_FAN_100}}      | {{D_FAN_100}}  | {{CT_FAN_100}}    | {{CD_FAN_100}} |
| Fan-out               | children=500   | {{C_FAN_500}}   | {{T_FAN_500}}      | {{D_FAN_500}}  | {{CT_FAN_500}}    | {{CD_FAN_500}} |
| Saga (happy)          | steps=10       | {{C_SAG_10}}    | {{T_SAG_10}}       | {{D_SAG_10}}   | {{CT_SAG_10}}     | {{CD_SAG_10}}  |
| Saga (happy)          | steps=100      | {{C_SAG_100}}   | {{T_SAG_100}}      | {{D_SAG_100}}  | {{CT_SAG_100}}    | {{CD_SAG_100}} |
| Saga (happy)          | steps=1000     | {{C_SAG_1000}}  | {{T_SAG_1000}}     | {{D_SAG_1000}} | {{CT_SAG_1000}}   | {{CD_SAG_1000}}|
| Saga (failure)        | steps=10       | {{C_SAGF_10}}   | {{T_SAGF_10}}      | {{D_SAGF_10}}  | {{CT_SAGF_10}}    | {{CD_SAGF_10}} |
| Saga (failure)        | steps=100      | {{C_SAGF_100}}  | {{T_SAGF_100}}     | {{D_SAGF_100}} | {{CT_SAGF_100}}   | {{CD_SAGF_100}}|
| LLM agent             | prompts=1x5    | {{C_LLM_1x5}}   | {{T_LLM_1x5}}      | {{D_LLM_1x5}}  | {{CT_LLM_1x5}}    | {{CD_LLM_1x5}} |
| LLM agent             | prompts=5x3    | {{C_LLM_5x3}}   | {{T_LLM_5x3}}      | {{D_LLM_5x3}}  | {{CT_LLM_5x3}}    | {{CD_LLM_5x3}} |
| LLM agent             | prompts=10x2   | {{C_LLM_10x2}}  | {{T_LLM_10x2}}     | {{D_LLM_10x2}} | {{CT_LLM_10x2}}   | {{CD_LLM_10x2}}|
| LLM agent             | prompts=50x1   | {{C_LLM_50x1}}  | {{T_LLM_50x1}}     | {{D_LLM_50x1}} | {{CT_LLM_50x1}}   | {{CD_LLM_50x1}}|

> **Note**: Ratios are computed as Cleat / Other. Values > 1.0 mean Cleat is
> faster. Values < 1.0 mean Cleat is slower. A "~" denotes no significant
> difference (ratio within 0.91-1.10).

---

## Per-Workload Detailed Results

### 1. Simple Sequential Workflow

N sequential no-op steps measuring pure framework overhead per durable call.

| Config  | Framework  | Run 1 (steps/s) | Run 2 (steps/s) | Run 3 (steps/s) | Median (steps/s) | vs Cleat |
|---------|------------|-----------------|-----------------|-----------------|------------------|----------|
| steps=10| Cleat      |                 |                 |                 |                  | 1.00     |
| steps=10| Temporal   |                 |                 |                 |                  |          |
| steps=10| DBOS       |                 |                 |                 |                  |          |
| steps=100| Cleat     |                 |                 |                 |                  | 1.00     |
| steps=100| Temporal  |                 |                 |                 |                  |          |
| steps=100| DBOS      |                 |                 |                 |                  |          |
| steps=1000| Cleat    |                 |                 |                 |                  | 1.00     |
| steps=1000| Temporal |                 |                 |                 |                  |          |
| steps=1000| DBOS     |                 |                 |                 |                  |          |

### 2. Fan-out Workflow

N concurrent child workflows spawned and awaited in parallel.

| Config      | Framework  | Run 1 (steps/s) | Run 2 (steps/s) | Run 3 (steps/s) | Median (steps/s) | vs Cleat |
|-------------|------------|-----------------|-----------------|-----------------|------------------|----------|
| children=10 | Cleat      |                 |                 |                 |                  | 1.00     |
| children=10 | Temporal   |                 |                 |                 |                  |          |
| children=10 | DBOS       |                 |                 |                 |                  |          |
| children=100| Cleat      |                 |                 |                 |                  | 1.00     |
| children=100| Temporal   |                 |                 |                 |                  |          |
| children=100| DBOS       |                 |                 |                 |                  |          |
| children=500| Cleat      |                 |                 |                 |                  | 1.00     |
| children=500| Temporal   |                 |                 |                 |                  |          |
| children=500| DBOS       |                 |                 |                 |                  |          |

### 3. Saga Compensation Workflow

#### Happy path (all steps succeed)

| Config  | Framework  | Run 1 (steps/s) | Run 2 (steps/s) | Run 3 (steps/s) | Median (steps/s) | vs Cleat |
|---------|------------|-----------------|-----------------|-----------------|------------------|----------|
| steps=10| Cleat      |                 |                 |                 |                  | 1.00     |
| steps=10| Temporal   |                 |                 |                 |                  |          |
| steps=10| DBOS       |                 |                 |                 |                  |          |
| steps=100| Cleat     |                 |                 |                 |                  | 1.00     |
| steps=100| Temporal  |                 |                 |                 |                  |          |
| steps=100| DBOS      |                 |                 |                 |                  |          |
| steps=1000| Cleat    |                 |                 |                 |                  | 1.00     |
| steps=1000| Temporal |                 |                 |                 |                  |          |
| steps=1000| DBOS     |                 |                 |                 |                  |          |

#### Failure path (last step fails, all previous compensated)

| Config  | Framework  | Run 1 (steps/s) | Run 2 (steps/s) | Run 3 (steps/s) | Median (steps/s) | vs Cleat |
|---------|------------|-----------------|-----------------|-----------------|------------------|----------|
| steps=10| Cleat      |                 |                 |                 |                  | 1.00     |
| steps=10| Temporal   |                 |                 |                 |                  |          |
| steps=10| DBOS       |                 |                 |                 |                  |          |
| steps=100| Cleat     |                 |                 |                 |                  | 1.00     |
| steps=100| Temporal  |                 |                 |                 |                  |          |
| steps=100| DBOS      |                 |                 |                 |                  |          |

### 4. LLM Agent Loop

Simulated AI agent with LLM chat calls and tool invocations per prompt.

| Config     | Framework  | Run 1 (steps/s) | Run 2 (steps/s) | Run 3 (steps/s) | Median (steps/s) | vs Cleat |
|------------|------------|-----------------|-----------------|-----------------|------------------|----------|
| prompts=1x5| Cleat      |                 |                 |                 |                  | 1.00     |
| prompts=1x5| Temporal   |                 |                 |                 |                  |          |
| prompts=1x5| DBOS       |                 |                 |                 |                  |          |
| prompts=5x3| Cleat      |                 |                 |                 |                  | 1.00     |
| prompts=5x3| Temporal   |                 |                 |                 |                  |          |
| prompts=5x3| DBOS       |                 |                 |                 |                  |          |
| prompts=10x2| Cleat     |                 |                 |                 |                  | 1.00     |
| prompts=10x2| Temporal  |                 |                 |                 |                  |          |
| prompts=10x2| DBOS      |                 |                 |                 |                  |          |
| prompts=50x1| Cleat     |                 |                 |                 |                  | 1.00     |
| prompts=50x1| Temporal  |                 |                 |                 |                  |          |
| prompts=50x1| DBOS      |                 |                 |                 |                  |          |

---

## Methodology Notes

- Tests run on the same hardware for all frameworks.
- Warm-up: {{WARMUP_DURATION}} (results discarded).
- Measurement window: {{MEASURE_DURATION}}.
- Each configuration run 3 times; median reported.
- CPU frequency governor set to `performance`, Turbo Boost disabled.
- No other user-space processes running during benchmarks.
- Cleat benchmarks run in-process with `go test -bench`.
- Temporal benchmarks run against {{TEMPORAL_SERVER_MODE}}.
- DBOS benchmarks run against PostgreSQL {{PG_VERSION}}.

---

## Raw Output

### Cleat

```
{{CLEAT_RAW_OUTPUT}}
```

### Temporal

```
{{TEMPORAL_RAW_OUTPUT}}
```

### DBOS

```
{{DBOS_RAW_OUTPUT}}
```
