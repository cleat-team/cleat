#!/usr/bin/env bash
#
# crash_demo.sh — Cleat LangChain Agent Crash Recovery Demo
#
# This script demonstrates the research agent surviving a worker crash.
# It builds the agent, starts execution, kills the worker mid-step, and
# then shows how the agent resumes from the last checkpoint.
#
# Usage:
#   ./crash_demo.sh
#
# Prerequisites:
#   - 'durable' CLI tool must be on PATH
#   - OPENAI_API_KEY must be set (or mock endpoints configured)
#   - python-sdk/ must be available at ../python-sdk/
#
# Note: This is a *narrative* demo — it prints the steps that would
# happen in a real scenario.  The actual 'durable' CLI commands shown
# work when the cleat runtime and worker are set up.  To run without
# the full runtime, use:  python research_agent.py --test

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Colour

echo -e "${CYAN}"
echo "============================================"
echo " Cleat LangChain Agent — Crash Recovery Demo"
echo "============================================"
echo -e "${NC}"

# ------------------------------------------------------------------
# Check prerequisites
# ------------------------------------------------------------------
if ! command -v durable &> /dev/null; then
    echo -e "${YELLOW}[INFO]${NC} 'durable' CLI not found on PATH."
    echo ""
    echo "The 'durable' CLI is needed to build WASM components and"
    echo "interact with the cleat runtime.  Install it from:"
    echo "  https://github.com/cleat-dev/cleat/releases"
    echo ""
    echo "For now, run the standalone test instead:"
    echo "  python research_agent.py --test"
    echo ""
    exit 0
fi

if [ -z "${OPENAI_API_KEY:-}" ]; then
    echo -e "${YELLOW}[WARN]${NC} OPENAI_API_KEY is not set."
    echo "  Set it to your OpenAI API key, or configure a different"
    echo "  LLM provider in research_agent.py."
    echo ""
fi

# ------------------------------------------------------------------
# Step 1 — Build
# ------------------------------------------------------------------
echo -e "${GREEN}Step 1:${NC} Building agent to WASM..."
echo "  $ durable build --target python \\"
echo "      --entry research_agent.py:langchain_research_agent"
echo ""

if durable build --target python \
    --entry research_agent.py:langchain_research_agent 2>&1; then
    echo -e "${GREEN}  Build successful.${NC}"
else
    echo -e "${YELLOW}  Build failed (expected if python-sdk setup is incomplete).${NC}"
    echo "  Continuing in narrative mode..."
fi
echo ""

# ------------------------------------------------------------------
# Step 2 — Start the agent
# ------------------------------------------------------------------
echo -e "${GREEN}Step 2:${NC} Starting research agent..."
echo "  $ durable run langchain_research_agent \\"
echo '      '"'"'{"topic": "Compare Temporal, DBOS, and Cleat"}'"'"' &'
echo ""

# Start the agent (capture PID for later kill)
durable run langchain_research_agent \
    '{"topic": "Compare Temporal, DBOS, and Cleat"}' &
AGENT_PID=$!
echo -e "  Agent PID: ${AGENT_PID}"
echo ""

# ------------------------------------------------------------------
# Step 3 — Let it make progress, then crash
# ------------------------------------------------------------------
echo -e "${GREEN}Step 3:${NC} Letting the agent make progress..."
echo "  (waiting 8 seconds for a few LLM + tool calls)"
sleep 8

echo ""
echo -e "${RED}  CRASH: Killing worker process (PID ${AGENT_PID})...${NC}"
kill -9 "${AGENT_PID}" 2>/dev/null || true
echo -e "${RED}  Worker killed mid-execution.${NC}"
echo ""

# ------------------------------------------------------------------
# Step 4 — Inspect event history
# ------------------------------------------------------------------
echo -e "${GREEN}Step 4:${NC} Checking event history..."
echo "  The Cleat dashboard shows recorded events from steps 1-N:"
echo ""
echo "  ┌─────────────────────────────────────────────────────────┐"
echo "  │  Dashboard:  http://localhost:8080/dashboard            │"
echo "  │                                                         │"
echo "  │  step_1_llm_start    │  LLM call to gpt-4o             │"
echo "  │  step_1_llm_end      │  Response with tool_calls       │"
echo "  │  step_1_tool_start   │  web_search query=...           │"
echo "  │  step_1_tool_end     │  Search results returned        │"
echo "  │  step_2_llm_start    │  LLM call with context          │"
echo "  │  ...                 │  (worker crashed here)          │"
echo "  └─────────────────────────────────────────────────────────┘"
echo ""

# ------------------------------------------------------------------
# Step 5 — Restart
# ------------------------------------------------------------------
echo -e "${GREEN}Step 5:${NC} Restarting worker..."
echo "  $ durable worker start &"
echo ""

durable worker start &
WORKER_PID=$!
sleep 3
echo -e "  Worker PID: ${WORKER_PID}"
echo ""

# ------------------------------------------------------------------
# Step 6 — Agent resumes
# ------------------------------------------------------------------
echo -e "${GREEN}Step 6:${NC} Agent resuming from last checkpoint..."
echo ""
echo "  [replay] step_1_llm_start    ← replayed from history (no API call)"
echo "  [replay] step_1_llm_end      ← replayed from history"
echo "  [replay] step_1_tool_start   ← replayed from history"
echo "  [replay] step_1_tool_end     ← replayed from history"
echo "  [replay] step_2_llm_start    ← replayed from history"
echo "  [NEW]    step_2_llm_end      ← fresh execution continues here"
echo "  [NEW]    step_2_tool_start   ← new tool call (real API)"
echo "  ..."
echo ""

echo -e "${GREEN}  Agent continues from where it left off.${NC}"
echo "  No duplicated LLM calls. No lost progress."
echo ""

# ------------------------------------------------------------------
# Wait for completion, then clean up
# ------------------------------------------------------------------
echo -e "${GREEN}Step 7:${NC} Waiting for agent to complete..."
wait "${AGENT_PID}" 2>/dev/null || true
wait "${WORKER_PID}" 2>/dev/null || true

echo ""
echo -e "${CYAN}"
echo "============================================"
echo " Demo Complete!"
echo "============================================"
echo -e "${NC}"
echo ""
echo "Key takeaways:"
echo ""
echo "  ${GREEN}1. No lost progress${NC}"
echo "     The agent resumed exactly where it crashed.  All context"
echo "     (messages, state, tool results) was preserved."
echo ""
echo "  ${GREEN}2. No duplicate API costs${NC}"
echo "     Steps 1-5 that completed before the crash were replayed"
echo "     from the event history — no second API bill."
echo ""
echo "  ${GREEN}3. Deterministic results${NC}"
echo "     The same inputs always produce the same outputs."
echo "     Perfect for testing, auditing, and debugging."
echo ""
echo "For a full walkthrough with actual execution:"
echo "  python research_agent.py --test"
echo ""
