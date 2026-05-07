"""
Hello World — Functional API host-side definitions.

Defines the LangGraph ``@task`` function that runs on the host side.
"""

from langgraph.func import entrypoint, task


@task
def process_query(query: str) -> str:
    """Process a query and return a response.

    This is a LangGraph ``@task`` that runs on the host side.
    It can use any Python library (LLM SDKs, databases, etc.).
    """
    return f"Processed: {query}"
