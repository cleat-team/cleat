<script lang="ts">
  import type { DAGSpec, DAGTask } from '../lib/types';

  let { dag }: { dag: DAGSpec } = $props();

  // ---------------------------------------------------------------
  // Layout computation: topological levels, then x/y positions.
  // ---------------------------------------------------------------
  interface LayoutNode extends DAGTask {
    level: number;
    x: number;
    y: number;
  }

  let layout = $derived.by<LayoutNode[]>(() => {
    const tasks = dag.tasks;
    const byName = new Map<string, DAGTask>();
    for (const t of tasks) byName.set(t.name, t);

    // Longest-path level assignment: a node's level = 1 + max(parent levels).
    const levelMemo = new Map<string, number>();
    function computeLevel(name: string): number {
      if (levelMemo.has(name)) return levelMemo.get(name)!;
      const task = byName.get(name);
      if (!task || !task.parents || task.parents.length === 0) {
        levelMemo.set(name, 0);
        return 0;
      }
      let maxParent = 0;
      for (const p of task.parents) {
        maxParent = Math.max(maxParent, computeLevel(p) + 1);
      }
      levelMemo.set(name, maxParent);
      return maxParent;
    }
    for (const t of tasks) computeLevel(t.name);

    // Group by level.
    const byLevel = new Map<number, LayoutNode[]>();
    for (const t of tasks) {
      const l = levelMemo.get(t.name) ?? 0;
      if (!byLevel.has(l)) byLevel.set(l, []);
      byLevel.get(l)!.push({ ...t, level: l, x: 0, y: 0 });
    }

    // Position nodes.
    const levels = [...byLevel.keys()].sort((a, b) => a - b);
    const boxW = 140;
    const boxH = 44;
    const padX = 40;
    const padY = 60;
    const marginX = 20;
    const marginY = 20;

    for (const [lvl, nodes] of byLevel) {
      const totalW = nodes.length * boxW + (nodes.length - 1) * padX;
      const startX = marginX + (totalW < 200 ? (200 - totalW) / 2 : 0);
      nodes.forEach((n, i) => {
        n.x = startX + i * (boxW + padX);
        n.y = marginY + lvl * (boxH + padY);
      });
    }

    const result: LayoutNode[] = [];
    for (const lvl of levels) {
      result.push(...(byLevel.get(lvl) || []));
    }
    return result;
  });

  let svgWidth = $derived.by<number>(() => {
    if (layout.length === 0) return 400;
    const maxX = Math.max(...layout.map(n => n.x + 160));
    return Math.max(400, maxX + 20);
  });

  let svgHeight = $derived.by<number>(() => {
    if (layout.length === 0) return 200;
    const maxY = Math.max(...layout.map(n => n.y + 64));
    return Math.max(200, maxY + 20);
  });

  // Build SVG edge paths (bezier curves from parent bottom-center to child top-center).
  interface Edge {
    path: string;
  }

  let edges = $derived.by<Edge[]>(() => {
    const byName = new Map<string, LayoutNode>();
    for (const n of layout) byName.set(n.name, n);

    const result: Edge[] = [];
    for (const n of layout) {
      if (!n.parents) continue;
      for (const p of n.parents) {
        const parent = byName.get(p);
        if (!parent) continue;

        const x1 = parent.x + 70;
        const y1 = parent.y + 44;
        const x2 = n.x + 70;
        const y2 = n.y;
        const cy = (y1 + y2) / 2;
        result.push({
          path: `M ${x1} ${y1} C ${x1} ${cy}, ${x2} ${cy}, ${x2} ${y2}`,
        });
      }
    }
    return result;
  });
</script>

<div class="dag-container">
  <h3>DAG: {dag.name}</h3>
  <svg width={svgWidth} height={svgHeight}
    style="display: block; background: var(--color-bg, #fafafa); border: 1px solid var(--color-border, #ddd); border-radius: 6px;">
    <defs>
      <marker id="arrowhead" markerWidth="10" markerHeight="7" refX="10" refY="3.5" orient="auto">
        <polygon points="0 0, 10 3.5, 0 7" fill="#888" />
      </marker>
    </defs>

    {#each edges as edge}
      <path d={edge.path} fill="none" stroke="#888" stroke-width="1.5" marker-end="url(#arrowhead)" />
    {/each}

    {#each layout as node}
      <g>
        <rect
          x={node.x}
          y={node.y}
          width="140"
          height="44"
          rx="6"
          ry="6"
          fill="var(--color-card-bg, #fff)"
          stroke="var(--color-primary, #4361ee)"
          stroke-width="2"
        />
        <text
          x={node.x + 70}
          y={node.y + 26}
          text-anchor="middle"
          dominant-baseline="middle"
          fill="var(--color-text, #333)"
          font-size="13"
          font-family="system-ui, sans-serif"
        >{node.name}</text>
      </g>
    {/each}

    {#if layout.length === 0}
      <text x="200" y="100" text-anchor="middle" fill="#999" font-size="14">No tasks to display</text>
    {/if}
  </svg>
</div>

<style>
  .dag-container {
    margin-top: 1rem;
  }
  .dag-container h3 {
    margin-bottom: 0.5rem;
    font-size: 0.95rem;
  }
</style>
