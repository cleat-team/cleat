"""Hello World workflow - demonstrates basic Cleat Python SDK usage."""
from dataclasses import dataclass
from cleat_sdk import HostCalls, durable_entry


@dataclass
class GreetingRequest:
    name: str
    language: str = "en"


@durable_entry
def hello_workflow(h: HostCalls, request: GreetingRequest) -> str:
    """A simple workflow that calls a greeting service."""
    h.durable_log(f"Hello workflow started for {request.name}")

    # Call an external translation/greeting service
    response = h.durable_call(
        "greeter", "Greet",
        {"name": request.name, "language": request.language}
    )

    h.durable_log(f"Got response: {response}")
    return response
