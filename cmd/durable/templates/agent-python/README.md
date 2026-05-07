# My Python Research Agent

A durable AI agent built with Cleat and LangChain. This agent researches
any topic using an LLM with web search and calculator tools.

## Quickstart

1. Install dependencies:
   ```bash
   pip install -r requirements.txt
   ```

2. Set your API keys:
   ```bash
   export OPENAI_API_KEY=sk-...
   ```

3. Run the agent:
   ```bash
   durable build --target python --entry agent.py:research_agent
   durable run research_agent '{"topic": "Latest developments in fusion energy"}'
   ```

## How It Works

The agent uses Cleat's durable execution to make every LLM call and tool
invocation survivable. If the worker crashes mid-research:

- **No progress is lost** — all completed steps are replayed from history
- **No duplicate API costs** — LLM responses are replayed, not re-fetched
- **Deterministic results** — the same inputs produce the same outputs

## Dashboard

Monitor your agent at `http://localhost:8080/dashboard` to see:
- LLM call history and costs
- Step-by-step execution trace
- Crash recovery events

## Customization

- Change the model: edit `agent.py` to use a different provider/model
- Add tools: define new tool functions and add them to the `tools` list
- Change the system prompt: edit `SYSTEM_PROMPT` in `agent.py`
